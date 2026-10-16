package notifications

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// CommitMetadata is returned by repo.GetCommitMetadata.
type CommitMetadata struct {
	// Message is the commit message.
	Message string
	// Author is the commit author.
	Author string
	// Date is the commit creation date.
	Date time.Time
	// Tags are the tags pointing at the commit.
	Tags []string
}

// HelmParameter is a Helm parameter.
type HelmParameter struct {
	Name        string
	Value       string
	ForceString bool
}

// HelmFileParameter is a Helm file parameter.
type HelmFileParameter struct {
	Name string
	Path string
}

// HelmAppSpec describes the Helm source of an application.
type HelmAppSpec struct {
	Name           string
	ValueFiles     []string
	Parameters     []HelmParameter
	Values         string
	FileParameters []HelmFileParameter
}

// CustomHelmAppSpec is the Helm detail returned by repo.GetAppDetails (field names follow
// the baseline so templates are portable).
type CustomHelmAppSpec struct {
	HelmAppSpec            HelmAppSpec
	HelmParameterOverrides []HelmParameter
}

// GetParameterValueByName returns the value of a parameter, preferring application
// overrides over chart values.
func (h CustomHelmAppSpec) GetParameterValueByName(name string) string {
	for _, p := range h.HelmParameterOverrides {
		if p.Name == name {
			return p.Value
		}
	}
	for _, p := range h.HelmAppSpec.Parameters {
		if p.Name == name {
			return p.Value
		}
	}
	return ""
}

// GetFileParameterPathByName returns the path of a file parameter.
func (h CustomHelmAppSpec) GetFileParameterPathByName(name string) string {
	for _, p := range h.HelmAppSpec.FileParameters {
		if p.Name == name {
			return p.Path
		}
	}
	return ""
}

// KustomizeAppSpec describes the Kustomize source of an application.
type KustomizeAppSpec struct {
	Images []string
}

// DirectoryAppSpec describes a plain directory source.
type DirectoryAppSpec struct{}

// AppDetail is returned by repo.GetAppDetails.
type AppDetail struct {
	Type      string
	Helm      *CustomHelmAppSpec
	Kustomize *KustomizeAppSpec
	Directory *DirectoryAppSpec
}

// RepoService gives trigger and template helpers access to repository information. The
// controller implements it on top of the repo-server client and repository credentials.
type RepoService interface {
	// GetCommitMetadata returns metadata of revision in repoURL, using the credentials the
	// application's project is allowed to use.
	GetCommitMetadata(ctx context.Context, repoURL, revision, project string) (*CommitMetadata, error)
	// GetAppDetails returns details of the application's first source.
	GetAppDetails(ctx context.Context, app map[string]any) (*AppDetail, error)
}

// ProjectGetter returns the AppProject an application belongs to, as an unstructured map.
type ProjectGetter interface {
	GetAppProject(ctx context.Context, name string) (map[string]any, error)
}

// ---------------------------------------------------------------------------------------
// Helper namespaces available to triggers (expr) and templates (text/template "call").

func timeHelpers(now func() time.Time) map[string]any {
	return map[string]any{
		"Parse":       parseTime,
		"Now":         now,
		"Nanosecond":  time.Nanosecond,
		"Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
		"Hour":        time.Hour,
		"Layout":      time.Layout,
		"ANSIC":       time.ANSIC,
		"UnixDate":    time.UnixDate,
		"RubyDate":    time.RubyDate,
		"RFC822":      time.RFC822,
		"RFC822Z":     time.RFC822Z,
		"RFC850":      time.RFC850,
		"RFC1123":     time.RFC1123,
		"RFC1123Z":    time.RFC1123Z,
		"RFC3339":     time.RFC3339,
		"RFC3339Nano": time.RFC3339Nano,
		"Kitchen":     time.Kitchen,
		"Stamp":       time.Stamp,
		"StampMilli":  time.StampMilli,
		"StampMicro":  time.StampMicro,
		"StampNano":   time.StampNano,
	}
}

// parseTime parses an RFC3339 timestamp.
func parseTime(timestamp string) (time.Time, error) {
	return time.Parse(time.RFC3339, timestamp)
}

var stringHelpers = map[string]any{
	"ReplaceAll": boundedReplaceAll,
	"ToUpper":    strings.ToUpper,
	"ToLower":    strings.ToLower,
}

// maxHelperOutputBytes bounds strings built by the strings.* helpers (triggers and
// templates): strings.ReplaceAll with an empty old string inserts new between every rune.
const maxHelperOutputBytes = 4 << 20

// boundedReplaceAll is strings.ReplaceAll refusing results larger than
// maxHelperOutputBytes (computed before allocating).
func boundedReplaceAll(s, old, replacement string) (string, error) {
	var n int
	if old == "" {
		n = utf8.RuneCountInString(s) + 1
	} else {
		n = strings.Count(s, old)
	}
	if grow := len(replacement) - len(old); n > 0 && grow > 0 && grow > (maxHelperOutputBytes-len(s))/n {
		return "", fmt.Errorf("%w: strings.ReplaceAll result exceeds %d bytes", ErrTemplateLimitExceeded, maxHelperOutputBytes)
	}
	return strings.ReplaceAll(s, old, replacement), nil
}

var syncHelpers = map[string]any{
	"GetInfoItem": getInfoItem,
}

func cloneHelpers(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// getInfoItem returns the value of the named info item of the application's sync
// operation (status.operationState.operation.info).
func getInfoItem(app map[string]any, name string) (string, error) {
	operation, ok := nestedMap(app, "status", "operationState", "operation")
	if !ok {
		return "", errors.New("application has no operation")
	}
	items, ok := operation["info"].([]any)
	if !ok {
		return "", errors.New("application sync operation has no info items")
	}
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if item["name"] == name {
			v, ok := item["value"].(string)
			if !ok {
				return "", fmt.Errorf("application has %q info item but it is not a string", name)
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("application sync operation has no info item with name %q", name)
}

// repoHelpers is created per Process call: it binds the call's context and application
// and memoizes repository lookups.
type repoHelpers struct {
	ctx     context.Context
	app     map[string]any
	svc     RepoService
	timeout time.Duration

	mu      sync.Mutex
	commits map[string]commitResult
	details *detailsResult
}

type commitResult struct {
	meta CommitMetadata
	err  error
}

type detailsResult struct {
	detail AppDetail
	err    error
}

func (r *repoHelpers) namespace() map[string]any {
	return map[string]any{
		"RepoURLToHTTPS":    services.RepoURLToHTTPS,
		"FullNameByRepoURL": fullNameByRepoURL,
		"QueryEscape":       url.QueryEscape,
		"GetCommitMetadata": r.getCommitMetadata,
		"GetAppDetails":     r.getAppDetails,
	}
}

func fullNameByRepoURL(raw string) (string, error) {
	if _, err := services.ParseGitURL(raw); err != nil {
		return "", err
	}
	return services.FullNameByRepoURL(raw), nil
}

func (r *repoHelpers) callCtx() (context.Context, context.CancelFunc) {
	if r.timeout > 0 {
		return context.WithTimeout(r.ctx, r.timeout)
	}
	return context.WithCancel(r.ctx)
}

func (r *repoHelpers) getCommitMetadata(sha string) (CommitMetadata, error) {
	if r.svc == nil {
		return CommitMetadata{}, ErrRepoServiceUnavailable
	}
	repoURL := appRepoURL(r.app)
	if repoURL == "" {
		return CommitMetadata{}, errors.New("failed to get application source repo URL")
	}
	project, _ := nestedString(r.app, "spec", "project")
	key := repoURL + "\x00" + sha
	r.mu.Lock()
	defer r.mu.Unlock()
	if res, ok := r.commits[key]; ok {
		return res.meta, res.err
	}
	ctx, cancel := r.callCtx()
	defer cancel()
	meta, err := r.svc.GetCommitMetadata(ctx, repoURL, sha, project)
	res := commitResult{err: err}
	if err == nil && meta != nil {
		res.meta = *meta
	} else if err == nil {
		res.err = errors.New("repository service returned no commit metadata")
	}
	if r.commits == nil {
		r.commits = map[string]commitResult{}
	}
	r.commits[key] = res
	return res.meta, res.err
}

func (r *repoHelpers) getAppDetails() (AppDetail, error) {
	if r.svc == nil {
		return AppDetail{}, ErrRepoServiceUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.details != nil {
		return r.details.detail, r.details.err
	}
	ctx, cancel := r.callCtx()
	defer cancel()
	d, err := r.svc.GetAppDetails(ctx, r.app)
	res := &detailsResult{err: err}
	if err == nil && d != nil {
		res.detail = *d
	} else if err == nil {
		res.err = errors.New("repository service returned no application details")
	}
	r.details = res
	return res.detail, res.err
}

// appRepoURL returns spec.source.repoURL, falling back to the first of spec.sources.
func appRepoURL(app map[string]any) string {
	if u, ok := nestedString(app, "spec", "source", "repoURL"); ok && u != "" {
		return u
	}
	spec, _ := app["spec"].(map[string]any)
	if sources, ok := spec["sources"].([]any); ok && len(sources) > 0 {
		if first, ok := sources[0].(map[string]any); ok {
			if u, ok := first["repoURL"].(string); ok {
				return u
			}
		}
	}
	return ""
}

func nestedMap(obj map[string]any, path ...string) (map[string]any, bool) {
	cur := obj
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func nestedString(obj map[string]any, path ...string) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	m, ok := nestedMap(obj, path[:len(path)-1]...)
	if !ok {
		return "", false
	}
	s, ok := m[path[len(path)-1]].(string)
	return s, ok
}

// deepCopyJSON copies a JSON-compatible value (maps, slices, scalars). Templates can call
// sprig functions that mutate dictionaries in place (set, unset, merge), so the engine never
// hands the caller's objects (often shared informer cache entries) to templates.
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyJSON(val)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, val := range t {
			out[k] = val
		}
		return out
	case []string:
		return append([]string(nil), t...)
	default:
		return v
	}
}

// stringMap converts a map[string]any of strings (unstructured labels/annotations).
func stringMap(v any) map[string]string {
	switch t := v.(type) {
	case map[string]string:
		return t
	case map[string]any:
		out := make(map[string]string, len(t))
		for k, val := range t {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

// SyncStatusRefreshed reports whether the application's sync status has been refreshed
// after its last completed operation. The baseline controller skips processing (returns
// false with a reason) while the operation finished after the last reconciliation, so
// sync-dependent triggers do not fire on stale status.
func SyncStatusRefreshed(app map[string]any) (bool, string) {
	op, ok := nestedMap(app, "status", "operationState")
	if !ok {
		return true, "no operation state"
	}
	phase, ok := op["phase"].(string)
	if !ok {
		return true, "no operation phase"
	}
	switch phase {
	case "Failed", "Error", "Succeeded":
	default:
		return true, "operation phase " + phase
	}
	finishedRaw, ok := op["finishedAt"].(string)
	if !ok {
		return false, "completed operation has no finishedAt"
	}
	finishedAt, err := time.Parse(time.RFC3339, finishedRaw)
	if err != nil {
		return false, "unparseable finishedAt"
	}
	var reconciledAt, observedAt time.Time
	if s, ok := nestedString(app, "status", "reconciledAt"); ok {
		reconciledAt, _ = time.Parse(time.RFC3339, s)
	}
	if s, ok := nestedString(app, "status", "observedAt"); ok {
		observedAt, _ = time.Parse(time.RFC3339, s)
	}
	if finishedAt.After(reconciledAt) && finishedAt.After(observedAt) {
		return false, "sync status out of date"
	}
	return true, "sync status up to date"
}
