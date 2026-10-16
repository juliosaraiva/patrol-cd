package webhook

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// secretSet is the verification material for one delivery.
type secretSet struct {
	// candidates are tried in order; a delivery is verified by any of them.
	candidates [][]byte
	// configured is true when at least one secret is configured for the provider, even
	// if it could not be resolved (a missing Secret fails closed).
	configured bool
	// unresolved counts configured SCMProvider secrets that could not be read.
	unresolved int
	// notLoaded is true while patrol-secret has never been loaded: its webhook secrets
	// are unknown rather than unset, so the provider counts as configured (fail closed).
	notLoaded bool
	// otherConfigured is true when a webhook secret is configured for any other
	// provider (patrol-secret key or trusted SCMProvider of any type). It decides the
	// unsigned-delivery policy when no secret is configured for this provider.
	otherConfigured bool
}

// anyPatrolSecret reports whether patrol-secret holds a webhook secret for any provider.
func anyPatrolSecret(w settings.Webhook) bool {
	for _, p := range providers {
		if len(p.secret(w)) > 0 {
			return true
		}
	}
	return false
}

// secretsFor collects the secrets verifying deliveries of p: the patrol-secret value
// first (unknown, and then failing closed, until patrol-secret has been loaded), then the
// webhook.secretRef of every SCMProvider of a matching type in the trusted namespaces,
// ordered by namespace and name. Several secrets support rotation (e.g. GitHub: old and
// new secret during a change). At most maxSecretCandidates are tried. It also reports
// whether any other provider has a secret (see secretSet.otherConfigured).
func (h *Handler) secretsFor(ctx context.Context, p *provider, w settings.Webhook) (secretSet, error) {
	var set secretSet
	if !w.SecretsLoaded {
		// patrol-secret has not been read yet: a secret may well be configured there, so
		// unsigned deliveries must not be accepted. SCMProvider secrets still verify.
		set.configured, set.notLoaded = true, true
	} else {
		if s := p.secret(w); len(s) > 0 {
			set.candidates = append(set.candidates, s)
			set.configured = true
		}
		set.otherConfigured = anyPatrolSecret(w)
	}
	if len(p.scmTypes) == 0 && (set.configured || set.otherConfigured || h.cfg.AllowUnsignedPerProvider || h.cfg.RequireSignatures) {
		// No SCMProvider can verify p, and none can change the unsigned policy decision.
		return set, nil
	}
	var providers []v1alpha1.SCMProvider
	for _, ns := range h.cfg.SCMProviderNamespaces {
		var list v1alpha1.SCMProviderList
		cctx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
		err := h.cfg.Reader.List(cctx, &list, client.InNamespace(ns))
		cancel()
		if err != nil {
			return set, fmt.Errorf("list SCMProviders in %s: %w", ns, err)
		}
		providers = append(providers, list.Items...)
	}
	slices.SortFunc(providers, func(a, b v1alpha1.SCMProvider) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for i := range providers {
		sp := &providers[i]
		if sp.Spec.Webhook == nil {
			continue
		}
		if !slices.Contains(p.scmTypes, sp.Spec.Type) {
			set.otherConfigured = true
			continue
		}
		set.configured = true
		if len(set.candidates) >= maxSecretCandidates {
			h.log.Warn("webhook: too many webhook secrets for provider; extra SCMProvider secrets ignored",
				"provider", p.name, "limit", maxSecretCandidates, "scmProvider", sp.Namespace+"/"+sp.Name)
			continue
		}
		val, err := h.readSecretKey(ctx, sp.Namespace, sp.Spec.Webhook.SecretRef)
		if err != nil {
			set.unresolved++
			h.log.Warn("webhook: SCMProvider webhook secret unavailable; deliveries it should verify are rejected",
				"provider", p.name, "scmProvider", sp.Namespace+"/"+sp.Name, "secret", sp.Spec.Webhook.SecretRef.Name, "key", sp.Spec.Webhook.SecretRef.Key, "error", logErr(err))
			continue
		}
		set.candidates = append(set.candidates, val)
	}
	return set, nil
}

// readSecretKey returns a non-empty value of a Secret key.
func (h *Handler) readSecretKey(ctx context.Context, ns string, ref v1alpha1.SecretKeyReference) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
	defer cancel()
	var sec corev1.Secret
	if err := h.cfg.Reader.Get(cctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %s/%s not found", ns, ref.Name)
		}
		return nil, fmt.Errorf("get secret %s/%s: %w", ns, ref.Name, err)
	}
	// Trimmed like the patrol-secret values (a file-created Secret often ends with a newline).
	v := bytes.TrimSpace(sec.Data[ref.Key])
	if len(v) == 0 {
		return nil, fmt.Errorf("key %q of secret %s/%s is missing or empty", ref.Key, ns, ref.Name)
	}
	return v, nil
}
