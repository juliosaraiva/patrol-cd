package reposerver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// integrityCheckGPG is the check name of GnuPG verification (upstream "GIT/GPG").
const integrityCheckGPG = "GIT/GPG"

// errStrictModeUnsupported is the problem reported for the "strict" GnuPG mode, whose
// history walk with seal commits is not implemented (fail closed).
const errStrictModeUnsupported = "GPG mode 'strict' (history verification) is not supported by this repo-server; the revision cannot be verified"

// verifyIntegrity verifies a checked-out git revision against the source integrity
// policy (upstream sourceintegrity.VerifyGit, head mode). It returns nil when no policy
// applies. unresolved is the requested revision: when it names a tag present in the
// working copy the tag object is verified (an annotated tag is signed separately),
// otherwise the commit sha. legacy is the deprecated one-line signature description.
func (s *Service) verifyIntegrity(ctx context.Context, gc *git.Client, si *v1alpha1.SourceIntegrity, repo apiclient.Repository, unresolved, sha string) (*apiclient.SourceIntegrityResult, string, error) {
	if si == nil {
		return nil, "", nil
	}
	repoURL := repo.URL
	v, err := project.GitVerificationFor(si, v1alpha1.ApplicationSource{RepoURL: repoURL}, !s.cfg.DisableGPG)
	if err != nil {
		return nil, "", err
	}
	if v == nil {
		return nil, "", nil
	}
	switch v.Mode {
	case v1alpha1.SourceIntegrityGitPolicyGPGModeHead:
	case v1alpha1.SourceIntegrityGitPolicyGPGModeStrict:
		s.log.Warn("reposerver: GPG strict mode requested but not supported; failing verification", slog.String("repo", git.SanitizeURL(repoURL)))
		return &apiclient.SourceIntegrityResult{Checks: []apiclient.SourceIntegrityCheck{{
			Name: integrityCheckGPG, Problems: []string{errStrictModeUnsupported},
		}}}, "", nil
	default:
		return nil, "", project.UnknownGPGModeError(v.Mode)
	}
	sig, err := s.signatureOf(ctx, gc, repo.Depth, unresolved, sha)
	if err != nil {
		return nil, "", err
	}
	problem := gpgProblem(v.Keys, sig)
	res := &apiclient.SourceIntegrityResult{Checks: []apiclient.SourceIntegrityCheck{{Name: integrityCheckGPG}}}
	if problem != "" {
		res.Checks[0].Problems = []string{problem}
	}
	return res, legacySignatureDescription(sig), nil
}

// revisionSignature is the verification outcome of one commit or tag.
type revisionSignature struct {
	revision string // commit SHA or tag name
	author   string
	info     git.SignatureInfo
}

// signatureOf verifies the annotated tag named unresolved when the requested revision
// is a remote tag (fetched when missing locally and the working copy is not shallow),
// else the commit sha.
func (s *Service) signatureOf(ctx context.Context, gc *git.Client, depth int64, unresolved, sha string) (revisionSignature, error) {
	home, release, err := s.keyring.acquire(ctx)
	if err != nil {
		return revisionSignature{}, fmt.Errorf("prepare GnuPG keyring: %w", err)
	}
	defer release()
	target, display := sha, sha
	if tag := s.remoteTag(ctx, gc, unresolved); tag != "" {
		target, display = "refs/tags/"+tag, tag
	}
	info, err := gc.VerifySignature(ctx, target, home)
	var nf *git.RevisionNotFoundError
	if target != sha && errors.As(err, &nf) && depth == 0 {
		// The commit was present, so the tag may not have been fetched yet.
		if ferr := s.fetch(ctx, gc, ""); ferr == nil {
			info, err = gc.VerifySignature(ctx, target, home)
		}
	}
	if target != sha && (errors.As(err, &nf) || errors.Is(err, git.ErrInvalidRevision)) {
		target, display = sha, sha
		info, err = gc.VerifySignature(ctx, target, home)
	}
	var sigErr *git.SignatureError
	switch {
	case err == nil:
	case errors.Is(err, git.ErrUnsigned):
		info = git.SignatureInfo{Result: git.SignatureUnsigned}
	case errors.As(err, &sigErr):
		info = sigErr.Info
	default:
		return revisionSignature{}, err
	}
	rs := revisionSignature{revision: display, info: info}
	if md, err := gc.RevisionMetadata(ctx, sha); err == nil {
		rs.author = md.Author
	} else {
		rs.author = info.Signer
	}
	return rs, nil
}

// remoteTag returns the tag name the requested revision denotes ("refs/tags/x" or a
// short name listed as a remote tag), or "".
func (s *Service) remoteTag(ctx context.Context, gc *git.Client, unresolved string) string {
	if t, ok := strings.CutPrefix(unresolved, "refs/tags/"); ok {
		return t
	}
	if unresolved == "" || unresolved == "HEAD" || strings.HasPrefix(unresolved, "refs/") || git.IsCommitSHA(unresolved) {
		return ""
	}
	refs, err := gc.LsRemote(ctx)
	if err != nil {
		return ""
	}
	if slices.Contains(refs.Tags, unresolved) {
		return unresolved
	}
	return ""
}

// gpgProblem describes a failed verification ("" when the signature is good and made
// with an allowed key), with upstream's wording. The signing key and the primary key
// of a signing subkey are both accepted.
func gpgProblem(allowed []string, sig revisionSignature) string {
	if sig.info.Result != git.SignatureGood {
		return fmt.Sprintf("Failed verifying revision %s by '%s': %s (key_id=%s)", sig.revision, sig.author, sig.info.Result, sig.info.KeyID)
	}
	primary := ""
	if len(sig.info.PrimaryFingerprint) >= 16 {
		primary = sig.info.PrimaryFingerprint[len(sig.info.PrimaryFingerprint)-16:]
	}
	for _, k := range allowed {
		id, err := settings.GPGKeyID(k)
		if err != nil {
			continue
		}
		if strings.EqualFold(id, sig.info.KeyID) || (primary != "" && strings.EqualFold(id, primary)) {
			return ""
		}
	}
	return fmt.Sprintf("Failed verifying revision %s by '%s': signed with unallowed key (key_id=%s)", sig.revision, sig.author, sig.info.KeyID)
}

// legacySignatureDescription is the deprecated signatureInfo/verifyResult text.
func legacySignatureDescription(sig revisionSignature) string {
	if sig.info.Result == git.SignatureUnsigned {
		return "Revision is not signed."
	}
	word := "Invalid"
	switch sig.info.Result {
	case git.SignatureGood:
		word = "Good"
	case git.SignatureBad:
		word = "Bad"
	}
	return fmt.Sprintf("%s signature from %s key %s", word, sig.author, sig.info.KeyID)
}

// integrityError converts problems of a result to an error (upstream AsError).
func integrityError(r *apiclient.SourceIntegrityResult) error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, c := range r.Checks {
		for _, p := range c.Problems {
			errs = append(errs, fmt.Errorf("%s: %s", c.Name, p))
		}
	}
	return errors.Join(errs...)
}
