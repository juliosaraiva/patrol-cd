package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// SignatureResult is the outcome of a GnuPG signature check. Values match
// upstream's GPGVerificationResult strings.
type SignatureResult string

// Signature results.
const (
	SignatureGood       SignatureResult = "signed"
	SignatureBad        SignatureResult = "bad signature"
	SignatureUntrusted  SignatureResult = "signed with untrusted key"
	SignatureExpired    SignatureResult = "expired signature"
	SignatureExpiredKey SignatureResult = "signed with expired key"
	SignatureRevokedKey SignatureResult = "signed with revoked key"
	SignatureMissingKey SignatureResult = "signed with key not in keyring"
	SignatureUnsigned   SignatureResult = "unsigned"
)

// SignatureInfo is the parsed `git verify-commit --raw` / `verify-tag --raw`
// status.
type SignatureInfo struct {
	Result SignatureResult
	// KeyID is the 16 hex character long key ID of the signing key (upper case).
	KeyID string
	// Fingerprint is the signing (sub)key fingerprint from VALIDSIG, when present.
	Fingerprint string
	// PrimaryFingerprint is the primary key fingerprint from VALIDSIG, when present.
	PrimaryFingerprint string
	// Signer is the user ID reported with the signature (may be empty).
	Signer string
	// Trust is the owner trust GnuPG reported (for example "ULTIMATE"), when present.
	Trust string
}

// maxStatusLines bounds the GnuPG status output that is parsed.
const maxStatusLines = 256

// VerifyCommitSignature verifies the GnuPG signature of revision (a commit, or an
// annotated tag object) against the keyring in gnupgHome and returns the signing
// key ID. It returns ErrUnsigned when there is no signature and a
// *SignatureError (ErrSignatureInvalid) when the signature is not good. Callers
// still check the key ID against their allow-list (AppProject signature keys).
func (c *Client) VerifyCommitSignature(ctx context.Context, revision, gnupgHome string) (string, error) {
	info, err := c.VerifySignature(ctx, revision, gnupgHome)
	if err != nil {
		return "", err
	}
	return info.KeyID, nil
}

// VerifySignature is VerifyCommitSignature returning the full parsed status.
func (c *Client) VerifySignature(ctx context.Context, revision, gnupgHome string) (SignatureInfo, error) {
	if err := c.requireRepo(); err != nil {
		return SignatureInfo{}, err
	}
	if err := validateRevisionArg(revision); err != nil {
		return SignatureInfo{}, err
	}
	if gnupgHome == "" || !filepath.IsAbs(gnupgHome) {
		return SignatureInfo{}, fmt.Errorf("git: GnuPG home must be an absolute path")
	}
	typ, err := c.git(ctx, cmdSpec{args: []string{"cat-file", "-t", revision}, maxStdout: maxSmallOutput})
	if err != nil {
		var exitErr *sandbox.ExitError
		if errors.As(err, &exitErr) && exitErr.Signal == 0 {
			return SignatureInfo{}, &RevisionNotFoundError{Revision: revision}
		}
		return SignatureInfo{}, err
	}
	sub := "verify-commit"
	switch strings.TrimSpace(typ) {
	case "commit":
	case "tag":
		sub = "verify-tag"
	default:
		return SignatureInfo{}, fmt.Errorf("%w: %s is a %s, not a commit or tag", ErrInvalidRevision,
			truncateForMessage(revision), strings.TrimSpace(typ))
	}
	res, runErr := c.run(ctx, cmdSpec{
		args:      []string{sub, "--raw", revision},
		env:       []string{"GNUPGHOME=" + gnupgHome},
		maxStdout: maxSmallOutput,
	})
	info, parsed := parseGPGStatus(string(res.Stderr))
	if !parsed {
		if runErr == nil {
			return SignatureInfo{}, fmt.Errorf("git: unexpected %s output for %s", sub, truncateForMessage(revision))
		}
		var ce *CommandError
		stderr := strings.TrimSpace(string(res.Stderr))
		if errors.As(runErr, &ce) && (stderr == "" || strings.Contains(stderr, "no signature found")) {
			return SignatureInfo{Result: SignatureUnsigned}, fmt.Errorf("%w: %s", ErrUnsigned, truncateForMessage(revision))
		}
		return SignatureInfo{}, runErr
	}
	if info.Result == SignatureGood && runErr != nil {
		// git refused a good signature (gpg.minTrustLevel): treat as untrusted.
		info.Result = SignatureUntrusted
	}
	if info.Result != SignatureGood {
		return info, &SignatureError{Revision: revision, Info: info}
	}
	return info, nil
}

// parseGPGStatus parses GnuPG --status-fd lines ("[GNUPG:] ..."). The first
// signature status line decides the result; VALIDSIG and TRUST_* lines enrich it.
func parseGPGStatus(out string) (SignatureInfo, bool) {
	var info SignatureInfo
	found := false
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for n := 0; sc.Scan() && n < maxStatusLines; n++ {
		rest, ok := strings.CutPrefix(sc.Text(), "[GNUPG:] ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		status := fields[0]
		var result SignatureResult
		switch status {
		case "GOODSIG":
			result = SignatureGood
		case "BADSIG":
			result = SignatureBad
		case "EXPSIG":
			result = SignatureExpired
		case "EXPKEYSIG":
			result = SignatureExpiredKey
		case "REVKEYSIG":
			result = SignatureRevokedKey
		case "ERRSIG":
			result = SignatureMissingKey
		case "VALIDSIG":
			if len(fields) > 1 {
				info.Fingerprint = strings.ToUpper(fields[1])
			}
			if len(fields) > 10 {
				info.PrimaryFingerprint = strings.ToUpper(fields[10])
			}
			continue
		default:
			if t, ok := strings.CutPrefix(status, "TRUST_"); ok && info.Trust == "" {
				info.Trust = t
			}
			continue
		}
		if found {
			continue
		}
		found = true
		info.Result = result
		if len(fields) > 1 && isHex(fields[1]) {
			info.KeyID = longKeyID(fields[1])
		}
		if status != "ERRSIG" && len(fields) > 2 {
			info.Signer = strings.Join(fields[2:], " ")
		}
	}
	if found && info.KeyID == "" && len(info.Fingerprint) >= 16 {
		info.KeyID = longKeyID(info.Fingerprint)
	}
	return info, found
}

// longKeyID returns the last 16 hex characters of a key ID or fingerprint, upper
// cased (upstream KeyID normalization).
func longKeyID(k string) string {
	k = strings.ToUpper(k)
	if len(k) > 16 {
		k = k[len(k)-16:]
	}
	return k
}
