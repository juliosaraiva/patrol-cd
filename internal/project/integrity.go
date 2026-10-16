package project

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// SignatureKeysMigration says how EffectiveSourceIntegrity used the deprecated
// spec.signatureKeys. Upstream logs a warning (Created, Merged) or an error (Ignored); the
// caller logs.
type SignatureKeysMigration int

const (
	// SignatureKeysNone: no signature keys; spec.sourceIntegrity is used as is.
	SignatureKeysNone SignatureKeysMigration = iota
	// SignatureKeysCreated: no sourceIntegrity; a policy was created from the keys.
	SignatureKeysCreated
	// SignatureKeysIgnored: sourceIntegrity.git is set; the keys are ignored.
	SignatureKeysIgnored
	// SignatureKeysMerged: sourceIntegrity without git; the converted policy was merged.
	SignatureKeysMerged
)

// RequiredSignatureKeys returns the deprecated spec.signatureKeys key IDs, in order.
func RequiredSignatureKeys(proj *v1alpha1.AppProject) []string {
	var keys []string
	if proj == nil {
		return nil
	}
	for _, k := range proj.Spec.SignatureKeys { //nolint:staticcheck // deprecated field, migrated (TEN-026)
		keys = append(keys, k.KeyID)
	}
	return keys
}

// EffectiveSourceIntegrity incorporates the deprecated signature keys into the source
// integrity policy (upstream AppProject.EffectiveSourceIntegrity, TEN-026, REPO-072): the
// keys become a single git policy {repos: [{url: "*"}], gpg: {mode: head, keys}} unless
// sourceIntegrity.git is already set, in which case they are ignored. proj is not modified;
// the result may share memory with proj only when no migration happened.
func EffectiveSourceIntegrity(proj *v1alpha1.AppProject) (*v1alpha1.SourceIntegrity, SignatureKeysMigration) {
	if proj == nil {
		return nil, SignatureKeysNone
	}
	keys := RequiredSignatureKeys(proj)
	if len(keys) == 0 {
		return proj.Spec.SourceIntegrity, SignatureKeysNone
	}
	migrated := &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: keys},
	}}}
	switch {
	case proj.Spec.SourceIntegrity == nil:
		return &v1alpha1.SourceIntegrity{Git: migrated}, SignatureKeysCreated
	case proj.Spec.SourceIntegrity.Git != nil:
		return proj.Spec.SourceIntegrity, SignatureKeysIgnored
	default:
		merged := proj.Spec.SourceIntegrity.DeepCopy()
		merged.Git = migrated
		return merged, SignatureKeysMerged
	}
}

// MatchGitIntegrityPolicies returns the git policies applying to repoURL (upstream
// findMatchingGitPolicies): a policy applies when one of its repos[].url globs matches the
// raw URL and no "!"-prefixed glob listed before a decision matches it (a negative match
// ends the evaluation of the policy). A policy without repos never applies.
func MatchGitIntegrityPolicies(git *v1alpha1.SourceIntegrityGit, repoURL string) []v1alpha1.SourceIntegrityGitPolicy {
	if git == nil {
		return nil
	}
	var out []v1alpha1.SourceIntegrityGitPolicy
	for _, p := range git.Policies {
		include := false
		for _, r := range p.Repos {
			if isDenyPattern(r.URL) {
				if globMatch(r.URL[1:], repoURL) {
					include = false
					break
				}
			} else if globMatch(r.URL, repoURL) {
				include = true
			}
		}
		if include {
			out = append(out, p)
		}
	}
	return out
}

// GitVerification is the verification a git source must pass.
type GitVerification struct {
	// Mode is "head" or "strict" (or an unknown value, which fails at verification with
	// UnknownGPGModeError).
	Mode v1alpha1.SourceIntegrityGitPolicyGPGMode
	// Keys are the trusted key IDs.
	Keys []string
}

// UnknownGPGModeError is the verification error for an unknown mode.
func UnknownGPGModeError(mode v1alpha1.SourceIntegrityGitPolicyGPGMode) error {
	return fmt.Errorf("unknown GPG mode %q configured for GIT source integrity", mode)
}

// GitVerificationFor returns the GPG verification the source must pass under si (upstream
// lookupGit), or nil when no check applies: not a git source (Helm chart, oci:// URL or
// empty source), no matching policy, a matching policy without gpg or with mode "none", or
// GPG disabled (gpgEnabled false, ARGOCD_GPG_ENABLED=false|no upstream). Several matching
// policies are a configuration error that fails closed with
// "multiple (<n>) git source integrity policies found for repo URL: <url>".
func GitVerificationFor(si *v1alpha1.SourceIntegrity, source v1alpha1.ApplicationSource, gpgEnabled bool) (*GitVerification, error) {
	if si == nil || si.Git == nil || !isGitSource(source) {
		return nil, nil
	}
	policies := MatchGitIntegrityPolicies(si.Git, source.RepoURL)
	switch len(policies) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, fmt.Errorf("multiple (%d) git source integrity policies found for repo URL: %s", len(policies), redactURL(source.RepoURL))
	}
	p := policies[0]
	if p.GPG == nil || p.GPG.Mode == v1alpha1.SourceIntegrityGitPolicyGPGModeNone || !gpgEnabled {
		return nil, nil
	}
	return &GitVerification{Mode: p.GPG.Mode, Keys: append([]string(nil), p.GPG.Keys...)}, nil
}

// HasSourceIntegrityCriteria reports whether any source must be verified under si (upstream
// HasCriteria): a sync with local manifests is then refused ("Cannot use local manifests when
// source integrity is enforced"). As upstream, a policy with mode "none" or without gpg is
// not a criterion, while ambiguous policies are.
func HasSourceIntegrityCriteria(si *v1alpha1.SourceIntegrity, gpgEnabled bool, sources ...v1alpha1.ApplicationSource) bool {
	for _, s := range sources {
		v, err := GitVerificationFor(si, s, gpgEnabled)
		if err != nil || v != nil {
			return true
		}
	}
	return false
}

// isGitSource mirrors upstream !source.IsZero() && !source.IsOCI() && !source.IsHelm().
func isGitSource(s v1alpha1.ApplicationSource) bool {
	if s.Chart != "" || strings.HasPrefix(s.RepoURL, "oci://") {
		return false
	}
	return !isZeroSource(s)
}

func isZeroSource(s v1alpha1.ApplicationSource) bool {
	return s.RepoURL == "" && s.Path == "" && s.TargetRevision == "" &&
		isZeroJSON(s.Helm) && isZeroJSON(s.Kustomize) && isZeroJSON(s.Directory) && isZeroJSON(s.Plugin)
}

// isZeroJSON reports whether v is nil or serializes to an empty object.
func isZeroJSON(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && (string(b) == "null" || string(b) == "{}")
}
