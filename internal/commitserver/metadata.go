package commitserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// MaxRenderedTemplateBytes bounds the output of the commit message and README templates.
const MaxRenderedTemplateBytes = 1 << 20

// CommitMetadata is a commit referenced by the dry commit's Argocd-reference-commit-*
// trailers. JSON names and Go field names are the template data contract shared with
// upstream templates (commit message: .metadata.references[].commit.author; README:
// $ref.Commit.SHA).
type CommitMetadata struct {
	Author  string `json:"author,omitempty"`
	Date    string `json:"date,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	SHA     string `json:"sha,omitempty"`
	RepoURL string `json:"repoUrl,omitempty"`
}

// RevisionReference is information related to the dry commit (only commits today).
type RevisionReference struct {
	Commit *CommitMetadata `json:"commit,omitempty"`
}

// HydratorCommitMetadata is the content of hydrator.metadata files and the data of the
// commit message template (as the JSON map under .metadata) and of the README template (as
// this struct, with only RepoURL, DrySHA and Commands set, as upstream).
type HydratorCommitMetadata struct {
	RepoURL  string   `json:"repoURL,omitempty"`
	DrySHA   string   `json:"drySha,omitempty"`
	Commands []string `json:"commands,omitempty"`
	Author   string   `json:"author,omitempty"`
	Date     string   `json:"date,omitempty"`
	// Subject is the first paragraph of the dry commit message.
	Subject string `json:"subject,omitempty"`
	// Body is the rest of the dry commit message without valid reference trailers.
	Body       string              `json:"body,omitempty"`
	References []RevisionReference `json:"references,omitempty"`
}

// NewCommitMetadata builds the hydrator metadata of a dry commit (upstream
// GetCommitMetadata): the subject is the message up to the first blank line, the body the
// rest without valid Argocd-reference-commit-* lines. md may be nil.
func NewCommitMetadata(logger *slog.Logger, repoURL, drySHA string, md *apiclient.RevisionMetadata) HydratorCommitMetadata {
	out := HydratorCommitMetadata{RepoURL: repoURL, DrySHA: drySHA}
	if md == nil {
		return out
	}
	out.Author = md.Author
	if !md.Date.IsZero() {
		out.Date = md.Date.UTC().Format(time.RFC3339)
	}
	subject, body, _ := strings.Cut(md.Message, "\n\n")
	out.Subject = subject
	if logger == nil {
		logger = slog.Default()
	}
	_, out.Body = git.ParseReferences(logger.With(slog.String("repo", git.SanitizeURL(repoURL)), slog.String("revision", drySHA)), body)
	for _, r := range md.References {
		c := &CommitMetadata{Author: r.Author, Subject: r.Subject, Body: r.Body, SHA: r.SHA, RepoURL: r.RepoURL}
		if !r.Date.IsZero() {
			c.Date = r.Date.Format(time.RFC3339)
		}
		if *c == (CommitMetadata{}) {
			continue
		}
		out.References = append(out.References, RevisionReference{Commit: c})
	}
	return out
}

// templateFuncs is Sprig's generic function map without the functions that reveal the
// environment (upstream removes env, expandenv and getHostByName).
func templateFuncs() template.FuncMap {
	f := sprig.GenericFuncMap()
	delete(f, "env")
	delete(f, "expandenv")
	delete(f, "getHostByName")
	return template.FuncMap(f)
}

// ParseTemplate parses a commit message or README template with the restricted function set.
func ParseTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Funcs(templateFuncs()).Parse(text)
}

// RenderCommitMessage renders the commit message template (patrol-cm
// sourceHydrator.commitMessageTemplate) with {"metadata": <md as JSON map>}. An empty
// template yields an empty message.
func RenderCommitMessage(tmpl string, md HydratorCommitMetadata) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return "", fmt.Errorf("failed to marshal commit metadata: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("failed to marshal commit metadata: %w", err)
	}
	t, err := ParseTemplate("commit-message", tmpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse commit message template: %w", err)
	}
	out, err := execute(t, map[string]any{"metadata": data})
	if err != nil {
		return "", fmt.Errorf("failed to execute commit message template: %w", err)
	}
	return out, nil
}

// renderReadme renders the README template of one path.
func renderReadme(t *template.Template, md HydratorCommitMetadata) ([]byte, error) {
	out, err := execute(t, md)
	if err != nil {
		return nil, fmt.Errorf("failed to execute readme template: %w", err)
	}
	return []byte(out), nil
}

var errTemplateOutputTooLarge = errors.New("template output exceeds the size limit")

func execute(t *template.Template, data any) (string, error) {
	w := &limitedBuffer{max: MaxRenderedTemplateBytes}
	if err := t.Execute(w, data); err != nil {
		return "", err
	}
	return w.buf.String(), nil
}

// limitedBuffer fails writes beyond max bytes, so a template cannot exhaust memory.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		return 0, errTemplateOutputTooLarge
	}
	return l.buf.Write(p)
}

// marshalMetadata encodes metadata as hydrator.metadata content: 2-space indented JSON
// without HTML escaping, followed by a newline (upstream json.Encoder settings).
func marshalMetadata(md HydratorCommitMetadata) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(md); err != nil {
		return nil, fmt.Errorf("failed to encode hydrator metadata: %w", err)
	}
	return buf.Bytes(), nil
}
