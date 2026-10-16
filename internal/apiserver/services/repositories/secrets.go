package repositories

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// secretOwner is the object a managed credentials Secret belongs to.
type secretOwner struct {
	kind       string // Repository or RepositoryCredential
	name       string
	uid        types.UID
	secretType string // cd.patrol.example/secret-type value
}

func (o secretOwner) ownerReference() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: o.kind, Name: o.name, UID: o.uid,
		Controller: ptr.To(true),
	}
}

// managedBy reports whether sec is a credentials Secret the API manages for owner.
func managedBy(sec *corev1.Secret, owner secretOwner) bool {
	if sec.Annotations[v1alpha1.AnnotationKeyManagedBy] != v1alpha1.AnnotationValueManagedByPatrol {
		return false
	}
	for _, ref := range sec.OwnerReferences {
		if ref.UID == owner.uid && ref.Kind == owner.kind {
			return true
		}
	}
	return false
}

// knownKeys returns the credential keys of data.
func knownKeys(data map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for _, k := range credentialKeys {
		if v, ok := data[k]; ok && len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

// getSecret returns the Secret, nil when it does not exist.
func (s *Service) getSecret(ctx context.Context, kc KubeClient, name string) (*corev1.Secret, error) {
	sec := &corev1.Secret{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sec, nil
}

// storedCredentials returns the credential keys stored for an object: the Secret named by
// ref when it is labelled with the owner's secret type (the repository library ignores
// other Secrets), otherwise none. Used to compare requests with stored objects; values are
// never returned to clients.
func (s *Service) storedCredentials(ctx context.Context, kc KubeClient, owner secretOwner, ref *v1alpha1.LocalSecretReference) (map[string][]byte, error) {
	if ref == nil {
		return map[string][]byte{}, nil
	}
	sec, err := s.getSecret(ctx, kc, ref.Name)
	if err != nil || sec == nil {
		return map[string][]byte{}, err
	}
	if sec.Labels[v1alpha1.LabelKeySecretType] != owner.secretType {
		return map[string][]byte{}, nil
	}
	return knownKeys(sec.Data), nil
}

// credentialPlan is a change of an object's credentials, computed from what is stored
// before anything is written. The object update that records ref() comes first (under the
// object's resourceVersion), applyCredentials second: a refused object update (409) leaves
// the stored credentials untouched.
type credentialPlan struct {
	owner secretOwner
	// name is the managed Secret written: the one the owner references when the API
	// manages it, else managedSecretName(owner).
	name string
	// base seeds a new managed Secret: the data of the Secret the owner referenced when the
	// API does not manage it but it carries the owner's secret type (keys the API does not
	// manage survive). That Secret itself is never modified.
	base map[string][]byte
	// creds are the recognized credential keys of the request (non-empty values).
	creds map[string][]byte
	// data is what the Secret holds after the change, computed from the state read by the
	// plan (empty: the Secret is deleted and the reference cleared).
	data map[string][]byte
}

// ref is the credentialsRef the owner records with the plan (nil when no data remains).
func (pl *credentialPlan) ref() *v1alpha1.LocalSecretReference {
	if len(pl.data) == 0 {
		return nil
	}
	return &v1alpha1.LocalSecretReference{Name: pl.name}
}

// merge replaces the recognized credential keys of existing by the request's.
func (pl *credentialPlan) merge(existing map[string][]byte) map[string][]byte {
	out := maps.Clone(existing)
	if out == nil {
		out = map[string][]byte{}
	}
	for _, k := range credentialKeys {
		delete(out, k)
	}
	maps.Copy(out, pl.creds)
	return out
}

// planCredentials computes how creds change the credentials of owner, whose current
// reference is current, without writing anything. A Secret named like the managed Secret
// that the API does not manage for owner is a 409.
func (s *Service) planCredentials(ctx context.Context, kc KubeClient, owner secretOwner, current *v1alpha1.LocalSecretReference, creds *apiclient.RepositoryCredentials) (*credentialPlan, error) {
	pl := &credentialPlan{owner: owner, name: managedSecretName(owner.name), creds: credentialData(creds)}
	if current != nil {
		sec, err := s.getSecret(ctx, kc, current.Name)
		if err != nil {
			return nil, err
		}
		if sec != nil {
			switch {
			case managedBy(sec, owner):
				pl.name = sec.Name
			case sec.Labels[v1alpha1.LabelKeySecretType] == owner.secretType:
				pl.base = sec.Data
			}
		}
	}
	existing := pl.base
	sec, err := s.getSecret(ctx, kc, pl.name)
	if err != nil {
		return nil, err
	}
	if sec != nil {
		if !managedBy(sec, owner) {
			return nil, errUnmanagedSecret(sec.Name, owner)
		}
		existing = sec.Data
	}
	pl.data = pl.merge(existing)
	return pl, nil
}

func errUnmanagedSecret(name string, owner secretOwner) error {
	return rest.Conflict("Secret %q already exists and is not managed for %s %q; delete or rename it", name, owner.kind, owner.name)
}

// applyCredentials writes a plan once the owner records pl.ref(): the managed Secret is
// created or updated (its recognized credential keys replaced, other keys kept), or deleted
// when no data remains. It returns the stored credential keys.
func (s *Service) applyCredentials(ctx context.Context, kc KubeClient, pl *credentialPlan) (map[string][]byte, error) {
	owner := pl.owner
	var stored map[string][]byte
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sec, err := s.getSecret(ctx, kc, pl.name)
		if err != nil {
			return err
		}
		if sec == nil {
			data := pl.merge(pl.base)
			stored = knownKeys(data)
			if len(data) == 0 {
				return nil
			}
			sec = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: pl.name, Namespace: s.cfg.Namespace,
					Labels:          map[string]string{v1alpha1.LabelKeySecretType: owner.secretType},
					Annotations:     map[string]string{v1alpha1.AnnotationKeyManagedBy: v1alpha1.AnnotationValueManagedByPatrol},
					OwnerReferences: []metav1.OwnerReference{owner.ownerReference()},
				},
				Type: corev1.SecretTypeOpaque,
				Data: data,
			}
			err := kc.Create(ctx, sec)
			if apierrors.IsAlreadyExists(err) {
				// Created concurrently: re-read and decide again.
				return apierrors.NewConflict(corev1.Resource("secrets"), pl.name, err)
			}
			return err
		}
		if !managedBy(sec, owner) {
			return errUnmanagedSecret(pl.name, owner)
		}
		data := pl.merge(sec.Data)
		stored = knownKeys(data)
		if len(data) == 0 {
			return client.IgnoreNotFound(kc.Delete(ctx, sec, client.Preconditions{UID: &sec.UID, ResourceVersion: &sec.ResourceVersion}))
		}
		sec.Data = data
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[v1alpha1.LabelKeySecretType] = owner.secretType
		return kc.Update(ctx, sec)
	})
	if err != nil {
		return nil, fmt.Errorf("store credentials of %s %q: %w", owner.kind, owner.name, err)
	}
	return stored, nil
}

// storeNewCredentials writes the credentials of an object that was just created with
// credentialsRef want (the Secret needs the object's UID for its ownerReference).
func (s *Service) storeNewCredentials(ctx context.Context, kc KubeClient, owner secretOwner, creds *apiclient.RepositoryCredentials, want *v1alpha1.LocalSecretReference) error {
	pl, err := s.planCredentials(ctx, kc, owner, nil, creds)
	if err != nil {
		return err
	}
	if ref := pl.ref(); ref == nil || want == nil || ref.Name != want.Name {
		return fmt.Errorf("credentials Secret of %s %q does not match its reference", owner.kind, owner.name)
	}
	_, err = s.applyCredentials(ctx, kc, pl)
	return err
}

// deleteManagedSecret deletes the credentials Secret named by ref when the API manages it
// for owner; other Secrets are kept.
func (s *Service) deleteManagedSecret(ctx context.Context, kc KubeClient, owner secretOwner, ref *v1alpha1.LocalSecretReference) error {
	if ref == nil {
		return nil
	}
	sec, err := s.getSecret(ctx, kc, ref.Name)
	if err != nil || sec == nil || !managedBy(sec, owner) {
		return err
	}
	return client.IgnoreNotFound(kc.Delete(ctx, sec, client.Preconditions{UID: &sec.UID}))
}
