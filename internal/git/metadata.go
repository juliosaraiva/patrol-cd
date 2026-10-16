package git

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Trailer keys of the source hydrator's commit references (upstream
// Argocd-reference-commit-*).
const (
	TrailerReferenceRepoURL = "Argocd-reference-commit-repourl"
	TrailerReferenceAuthor  = "Argocd-reference-commit-author"
	TrailerReferenceDate    = "Argocd-reference-commit-date"
	TrailerReferenceSubject = "Argocd-reference-commit-subject"
	TrailerReferenceBody    = "Argocd-reference-commit-body"
	TrailerReferenceSHA     = "Argocd-reference-commit-sha"
)

// CommitMetadata describes a commit referenced from another commit's trailers.
type CommitMetadata struct {
	// Author is the RFC 5322 address of the author ("Name <email>").
	Author string
	// Date is RFC 3339, or "" when unknown.
	Date string
	// Subject is the referenced commit's subject line.
	Subject string
	// Body is the referenced commit's body (JSON-decoded from the trailer).
	Body string
	// SHA is the referenced commit (5 to 40 lowercase hex characters).
	SHA string
	// RepoURL is the repository of the referenced commit. It is only checked to be
	// a parseable URL; callers must validate it before building links.
	RepoURL string
}

// RevisionReference is information related to a commit. Only commit references
// exist today.
type RevisionReference struct {
	Commit *CommitMetadata
}

// RevisionMetadata is the metadata of one commit.
type RevisionMetadata struct {
	// Author is "Name <email>" as `git show --format='%an <%ae>'`.
	Author string
	// Date is the author date.
	Date time.Time
	// Tags lists the tags pointing at the commit.
	Tags []string
	// Message is the full commit message, trimmed.
	Message string
	// References are parsed from valid Argocd-reference-commit-* trailers.
	References []RevisionReference
	// Signature is set when the client was created with GnuPGHome.
	Signature *SignatureInfo
}

// RevisionMetadata returns author, date, tags, message, trailer references and
// (with GnuPGHome) signature information of revision, which must be present in
// the working copy.
func (c *Client) RevisionMetadata(ctx context.Context, revision string) (*RevisionMetadata, error) {
	if err := c.requireRepo(); err != nil {
		return nil, err
	}
	if err := validateRevisionArg(revision); err != nil {
		return nil, err
	}
	out, err := c.git(ctx, cmdSpec{
		args:      []string{"show", "-s", "--no-color", "--format=%an <%ae>%n%at%n%B", revision, "--"},
		maxStdout: maxMetadataOutput,
	})
	if err != nil {
		return nil, err
	}
	segments := strings.SplitN(out, "\n", 3)
	if len(segments) < 2 {
		return nil, fmt.Errorf("git: unexpected show output for %s", truncateForMessage(revision))
	}
	author := segments[0]
	ts, _ := strconv.ParseInt(strings.TrimSpace(segments[1]), 10, 64)
	message := ""
	if len(segments) == 3 {
		message = strings.TrimSpace(segments[2])
	}

	trailers, err := c.git(ctx, cmdSpec{
		args:      []string{"interpret-trailers", "--parse"},
		stdin:     strings.NewReader(message),
		maxStdout: maxMetadataOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to interpret trailers for revision %q: %w", truncateForMessage(revision), err)
	}
	refs, _ := ParseReferences(c.log.With(slog.String("repo", c.RepoURL())), trailers)

	tagsOut, err := c.git(ctx, cmdSpec{args: []string{"tag", "--points-at", revision}, maxStdout: maxMetadataOutput})
	if err != nil {
		return nil, err
	}
	m := &RevisionMetadata{
		Author:     author,
		Date:       time.Unix(ts, 0),
		Tags:       strings.Fields(tagsOut),
		Message:    message,
		References: refs,
	}
	if c.opts.gnupgHome != "" {
		info, err := c.VerifySignature(ctx, revision, c.opts.gnupgHome)
		var sigErr *SignatureError
		switch {
		case err == nil:
			m.Signature = &info
		case errors.Is(err, ErrUnsigned):
			m.Signature = &SignatureInfo{Result: SignatureUnsigned}
		case errors.As(err, &sigErr):
			m.Signature = &sigErr.Info
		default:
			return nil, err
		}
	}
	return m, nil
}

var referenceSHARe = regexp.MustCompile(`^[0-9a-f]{5,40}$`)

// ParseReferences extracts commit references from commit trailers, as upstream
// GetReferences: every valid Argocd-reference-commit-* line updates a single
// CommitMetadata (invalid values are logged and kept as unrelated lines). It
// returns nil when no reference was found, and the input without the valid
// reference lines.
func ParseReferences(logger *slog.Logger, trailers string) ([]RevisionReference, string) {
	if logger == nil {
		logger = slog.Default()
	}
	var unrelated strings.Builder
	var meta CommitMetadata
	sc := bufio.NewScanner(strings.NewReader(trailers))
	sc.Buffer(make([]byte, 0, 64<<10), int(maxMetadataOutput))
	for sc.Scan() {
		line := sc.Text()
		if !updateReference(logger, &meta, line) {
			unrelated.WriteString(line)
			unrelated.WriteByte('\n')
		}
	}
	if meta == (CommitMetadata{}) {
		return nil, unrelated.String()
	}
	return []RevisionReference{{Commit: &meta}}, unrelated.String()
}

func updateReference(logger *slog.Logger, meta *CommitMetadata, line string) bool {
	if !strings.HasPrefix(line, "Argocd-reference-commit-") {
		return false
	}
	key, value, ok := strings.Cut(line, ": ")
	if !ok {
		return false
	}
	invalid := func(what string, err error) bool {
		logger.Error("git: invalid commit reference trailer", slog.String("trailer", key),
			slog.String("value", truncateRunes(value, 100)), slog.String("problem", what), slog.Any("error", err))
		return false
	}
	switch key {
	case TrailerReferenceRepoURL:
		if _, err := url.Parse(value); err != nil {
			return invalid("unparseable URL", err)
		}
		meta.RepoURL = value
	case TrailerReferenceAuthor:
		addr, err := mail.ParseAddress(value)
		if err != nil || addr == nil {
			return invalid("unparseable author address", err)
		}
		meta.Author = addr.String()
	case TrailerReferenceDate:
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return invalid("date is not RFC 3339", err)
		}
		meta.Date = t.Format(time.RFC3339)
	case TrailerReferenceSubject:
		meta.Subject = value
	case TrailerReferenceBody:
		var body string
		if err := json.Unmarshal([]byte(value), &body); err != nil {
			return invalid("body is not a JSON string", err)
		}
		meta.Body = body
	case TrailerReferenceSHA:
		if !referenceSHARe.MatchString(value) {
			return invalid("SHA must be 5-40 lowercase hex characters", nil)
		}
		meta.SHA = value
	default:
		return false
	}
	return true
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
