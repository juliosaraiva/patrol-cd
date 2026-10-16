package gitlab_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitLab is an in-process GitLab REST API v4 subset with offset pagination
// (X-Next-Page and Link headers), PRIVATE-TOKEN authentication and rate-limit
// injection. It serves realistic payload shapes for the endpoints the provider uses.
type fakeGitLab struct {
	srv *httptest.Server

	mu        sync.Mutex
	token     string
	maxPer    int
	groups    map[string]int64 // full path → id
	projects  []*fgProject
	limitTill time.Time
	requests  []string
}

type fgBranch struct {
	Name, SHA string
	Protected bool
}

type fgMR struct {
	IID          int64
	Title        string
	Source       string
	Target       string
	SHA          string
	Author       string
	Labels       []string
	State        string // opened, closed, merged, locked
	Draft        bool
	TargetProjID int64
}

type fgProject struct {
	ID            int64
	Namespace     string
	Path          string
	DefaultBranch string
	Archived      bool
	Topics        []string
	TagList       []string
	Visibility    string
	SharedWith    []string
	Branches      []fgBranch
	Tags          map[string]string
	Files         map[string][]string // sha → files
	MRs           []fgMR
}

func (p *fgProject) fullPath() string { return p.Namespace + "/" + p.Path }

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{maxPer: 3, groups: map[string]int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) url() string { return f.srv.URL }

func (f *fakeGitLab) addGroup(path string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[path] = id
}

func (f *fakeGitLab) addProject(p *fgProject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = append(f.projects, p)
}

func (f *fakeGitLab) injectRateLimit(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitTill = time.Now().Add(d)
}

func (f *fakeGitLab) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 " + what + " Not Found"})
}

// segments splits the escaped path below /api/v4/ into unescaped segments, so that a
// URL-encoded project path ("acme%2Fplatform") stays one segment.
func segments(r *http.Request) []string {
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/")
	var out []string
	for s := range strings.SplitSeq(p, "/") {
		u, err := url.PathUnescape(s)
		if err != nil {
			u = s
		}
		out = append(out, u)
	}
	return out
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if !strings.HasPrefix(r.URL.Path, "/api/v4/") {
		notFound(w, "Path")
		return
	}
	if time.Now().Before(f.limitTill) {
		reset := f.limitTill.Unix()
		w.Header().Set("RateLimit-Limit", "600")
		w.Header().Set("RateLimit-Remaining", "0")
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(f.limitTill).Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "Retry later"})
		return
	}
	if f.token != "" && r.Header.Get("PRIVATE-TOKEN") != f.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "405 Method Not Allowed"})
		return
	}
	seg := segments(r)
	q := r.URL.Query()
	switch {
	case len(seg) == 3 && seg[0] == "groups" && seg[2] == "projects":
		f.groupProjects(w, r, seg[1], q)
	case len(seg) >= 2 && seg[0] == "projects":
		p := f.project(seg[1])
		if p == nil {
			notFound(w, "Project")
			return
		}
		f.projectRoute(w, r, p, seg[2:], q)
	default:
		notFound(w, "Path")
	}
}

func (f *fakeGitLab) project(id string) *fgProject {
	for _, p := range f.projects {
		if strconv.FormatInt(p.ID, 10) == id || strings.EqualFold(p.fullPath(), id) {
			return p
		}
	}
	return nil
}

func (f *fakeGitLab) groupProjects(w http.ResponseWriter, r *http.Request, gid string, q url.Values) {
	var group string
	for path, id := range f.groups {
		if strings.EqualFold(path, gid) || strconv.FormatInt(id, 10) == gid {
			group = path
		}
	}
	if group == "" {
		notFound(w, "Group")
		return
	}
	subgroups := q.Get("include_subgroups") == "true"
	shared := q.Get("with_shared") != "false"
	var topics []string
	if t := q.Get("topic"); t != "" {
		topics = strings.Split(t, ",")
	}
	var out []map[string]any
	for _, p := range f.projects {
		in := strings.EqualFold(p.Namespace, group) ||
			(subgroups && strings.HasPrefix(strings.ToLower(p.Namespace), strings.ToLower(group)+"/")) ||
			(shared && slices.ContainsFunc(p.SharedWith, func(g string) bool { return strings.EqualFold(g, group) }))
		if !in {
			continue
		}
		// GitLab API: archived=true → only archived, archived=false → non-archived,
		// omitted → every project.
		switch q.Get("archived") {
		case "true":
			if !p.Archived {
				continue
			}
		case "false":
			if p.Archived {
				continue
			}
		}
		if !containsAll(p.Topics, topics) {
			continue
		}
		out = append(out, f.projectJSON(p))
	}
	paginate(w, r, out, f.maxPer)
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func (f *fakeGitLab) projectJSON(p *fgProject) map[string]any {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	var def any
	if p.DefaultBranch != "" {
		def = p.DefaultBranch
	}
	vis := p.Visibility
	if vis == "" {
		vis = "private"
	}
	topics := p.Topics
	if topics == nil {
		topics = []string{}
	}
	return map[string]any{
		"id":                  p.ID,
		"description":         "",
		"name":                p.Path,
		"name_with_namespace": strings.ReplaceAll(p.fullPath(), "/", " / "),
		"path":                p.Path,
		"path_with_namespace": p.fullPath(),
		"created_at":          "2024-01-01T00:00:00.000Z",
		"default_branch":      def,
		"tag_list":            p.TagList,
		"topics":              topics,
		"ssh_url_to_repo":     "git@" + host + ":" + p.fullPath() + ".git",
		"http_url_to_repo":    f.srv.URL + "/" + p.fullPath() + ".git",
		"web_url":             f.srv.URL + "/" + p.fullPath(),
		"archived":            p.Archived,
		"visibility":          vis,
		"empty_repo":          len(p.Branches) == 0,
		"namespace": map[string]any{
			"id": 1, "name": p.Namespace, "path": p.Namespace, "kind": "group", "full_path": p.Namespace,
		},
	}
}

func (f *fakeGitLab) branch(p *fgProject, name string) (fgBranch, bool) {
	for _, b := range p.Branches {
		if b.Name == name {
			return b, true
		}
	}
	return fgBranch{}, false
}

func branchJSON(p *fgProject, b fgBranch) map[string]any {
	return map[string]any{
		"name":      b.Name,
		"merged":    false,
		"protected": b.Protected,
		"default":   b.Name == p.DefaultBranch,
		"commit": map[string]any{
			"id": b.SHA, "short_id": b.SHA[:8], "title": "commit " + b.SHA[:8],
		},
	}
}

// resolve resolves a branch, tag or SHA.
func (f *fakeGitLab) resolve(p *fgProject, ref string) (string, bool) {
	if ref == "HEAD" {
		ref = p.DefaultBranch
	}
	if b, ok := f.branch(p, ref); ok {
		return b.SHA, true
	}
	if s, ok := p.Tags[ref]; ok {
		return s, true
	}
	for _, b := range p.Branches {
		if b.SHA == ref {
			return ref, true
		}
	}
	if _, ok := p.Files[ref]; ok {
		return ref, true
	}
	return "", false
}

func (f *fakeGitLab) projectRoute(w http.ResponseWriter, r *http.Request, p *fgProject, rest []string, q url.Values) {
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, f.projectJSON(p))
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "branches":
		var out []map[string]any
		for _, b := range p.Branches {
			out = append(out, branchJSON(p, b))
		}
		paginate(w, r, out, f.maxPer)
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches":
		b, ok := f.branch(p, rest[2])
		if !ok {
			notFound(w, "Branch")
			return
		}
		writeJSON(w, http.StatusOK, branchJSON(p, b))
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "commits":
		sha, ok := f.resolve(p, rest[2])
		if !ok {
			notFound(w, "Commit")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": sha, "short_id": sha[:8], "title": "x", "parent_ids": []string{}})
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "files":
		sha, ok := f.resolve(p, q.Get("ref"))
		if !ok || !slices.Contains(p.Files[sha], rest[2]) {
			notFound(w, "File")
			return
		}
		w.Header().Set("X-Gitlab-File-Path", rest[2])
		w.Header().Set("X-Gitlab-Commit-Id", sha)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"file_path": rest[2], "ref": q.Get("ref"), "content": "", "encoding": "base64"})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "tree":
		sha, ok := f.resolve(p, q.Get("ref"))
		if !ok {
			notFound(w, "Tree")
			return
		}
		dir := strings.Trim(q.Get("path"), "/")
		var out []map[string]any
		for _, file := range p.Files[sha] {
			if dir == "" || strings.HasPrefix(file, dir+"/") {
				out = append(out, map[string]any{"name": file, "type": "blob", "path": file})
			}
		}
		if len(out) == 0 && dir != "" {
			notFound(w, "Tree")
			return
		}
		paginate(w, r, out, f.maxPer)
	case len(rest) == 1 && rest[0] == "merge_requests":
		state := q.Get("state")
		var labels []string
		if l := q.Get("labels"); l != "" {
			labels = strings.Split(l, ",")
		}
		var out []map[string]any
		for _, mr := range p.MRs {
			if state != "" && state != "all" && mr.State != state {
				continue
			}
			if !matchLabels(mr.Labels, labels) {
				continue
			}
			if tb := q.Get("target_branch"); tb != "" && mr.Target != tb {
				continue
			}
			out = append(out, mrJSON(f.srv.URL, p, mr))
		}
		paginate(w, r, out, f.maxPer)
	default:
		notFound(w, "Path")
	}
}

// matchLabels applies GitLab's documented merge request "labels" filter: the
// predefined keywords None (no labels) and Any (at least one label) are
// case-insensitive; otherwise every listed label is required.
func matchLabels(have, want []string) bool {
	for _, w := range want {
		switch strings.ToLower(w) {
		case "none":
			return len(have) == 0
		case "any":
			return len(have) > 0
		}
	}
	return containsAll(have, want)
}

func mrJSON(base string, p *fgProject, mr fgMR) map[string]any {
	labels := mr.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id":                1000 + mr.IID,
		"iid":               mr.IID,
		"project_id":        p.ID,
		"title":             mr.Title,
		"state":             mr.State,
		"target_branch":     mr.Target,
		"source_branch":     mr.Source,
		"source_project_id": p.ID,
		"target_project_id": p.ID,
		"labels":            labels,
		"draft":             mr.Draft,
		"work_in_progress":  mr.Draft,
		"sha":               mr.SHA,
		"web_url":           fmt.Sprintf("%s/%s/-/merge_requests/%d", base, p.fullPath(), mr.IID),
		"author":            map[string]any{"id": 7, "username": mr.Author, "name": strings.ToUpper(mr.Author[:1]) + mr.Author[1:], "state": "active"},
	}
}

// paginate serves items with GitLab offset pagination headers.
func paginate(w http.ResponseWriter, r *http.Request, items []map[string]any, maxPer int) {
	per := maxPer
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 && v < per {
		per = v
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	start := min((page-1)*per, len(items))
	end := min(start+per, len(items))
	totalPages := max(1, (len(items)+per-1)/per)
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Per-Page", strconv.Itoa(per))
	w.Header().Set("X-Total", strconv.Itoa(len(items)))
	w.Header().Set("X-Total-Pages", strconv.Itoa(totalPages))
	next := ""
	if page < totalPages {
		next = strconv.Itoa(page + 1)
		u := *r.URL
		qq := u.Query()
		qq.Set("page", next)
		u.RawQuery = qq.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, u.RequestURI()))
	}
	w.Header().Set("X-Next-Page", next)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	page1 := items[start:end]
	if page1 == nil {
		page1 = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, page1)
}
