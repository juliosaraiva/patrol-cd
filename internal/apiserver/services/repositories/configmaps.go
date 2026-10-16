package repositories

import (
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
)

// trustConfigMap is a read trust ConfigMap (certificates, known hosts, GnuPG keys).
type trustConfigMap struct {
	name string
	cm   *corev1.ConfigMap // nil when it does not exist
}

func (t trustConfigMap) data() map[string]string {
	if t.cm == nil {
		return map[string]string{}
	}
	return maps.Clone(t.cm.Data)
}

// readTrustConfigMap reads a trust ConfigMap. A missing one is empty; one without the
// app.kubernetes.io/part-of=patrol-cd label is refused with 412, because the settings
// layer ignores it (its content has no effect, and writing to it would not either).
func (s *Service) readTrustConfigMap(ctx context.Context, kc KubeClient, name string) (trustConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return trustConfigMap{name: name}, nil
		}
		return trustConfigMap{}, err
	}
	if cm.Labels[v1alpha1.LabelKeyPartOf] != v1alpha1.LabelValuePartOf {
		return trustConfigMap{}, rest.FailedPrecondition("ConfigMap %s is ignored because it is not labelled %s=%s; label it or delete it", name, v1alpha1.LabelKeyPartOf, v1alpha1.LabelValuePartOf)
	}
	return trustConfigMap{name: name, cm: cm}, nil
}

// writeTrustConfigMap stores data in the ConfigMap read as t: an update carrying t's
// resourceVersion (a Conflict when it changed meanwhile), or a create (labelled) when it
// did not exist (a Conflict when it was created meanwhile).
func (s *Service) writeTrustConfigMap(ctx context.Context, kc KubeClient, t trustConfigMap, data map[string]string) error {
	if t.cm == nil {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: t.name, Namespace: s.cfg.Namespace,
				Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf},
			},
			Data: data,
		}
		err := kc.Create(ctx, cm)
		if apierrors.IsAlreadyExists(err) {
			return apierrors.NewConflict(corev1.Resource("configmaps"), t.name, err)
		}
		if err == nil {
			targetConfigMap(ctx, cm)
		}
		return err
	}
	cm := t.cm.DeepCopy()
	cm.Data = data
	if err := kc.Update(ctx, cm); err != nil {
		return err
	}
	targetConfigMap(ctx, cm)
	return nil
}

func targetConfigMap(ctx context.Context, cm *corev1.ConfigMap) {
	rest.AuditFrom(ctx).SetTarget(rest.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: cm.Namespace, Name: cm.Name, UID: cm.UID})
}

// retryOnConflict re-runs a read-modify-write on optimistic concurrency conflicts.
func retryOnConflict(fn func() error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, fn)
}
