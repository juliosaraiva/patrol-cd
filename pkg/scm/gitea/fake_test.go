package gitea_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitea is an in-process Gitea API v1 subset with page/limit pagination (Link and
// X-Total-Count headers), token authentication and rate-limit injection.
type fakeGitea struct {
	srv *httptest.Server

	mu        sync.Mutex
	token     string
	maxPer    int
	noLink    bool
	orgs      map[string]bool
	users     map[string]bool
	repos     []*ftRepo
	limitTill time.Time
	requests  []string
}

type ftBranch struct{ Name, SHA string }

type ftPull struct {
	Number        int64
	Title         string
	Head, Base    string
	SHA           string
	User          string
	Labels        []string
	State         string
	Merged, Draft bool
}

type ftRepo struct {
	ID            int64
	Owner, Name   string
	DefaultBranch string
	Archived      bool
	Empty         bool
	Private       bool
	Topics        []string
	Labels        []string
	Branches      []ftBranch
	Tags          map[string]string
	Files         map[string][]string
	Pulls         []ftPull
}

func newFakeGitea(t *testing.T) *fakeGitea {
	t.Helper()
	f := &fakeGitea{maxPer: 3, orgs: map[string]bool{}, users: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitea) add(r *ftRepo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, r)
}

func (f *fakeGitea) injectRateLimit(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitTill = time.Now().Add(d)
}

func (f *fakeGitea) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"errors": []string{}, "message": "The target couldn't be found.", "url": "https://gitea.example.com/api/swagger"})
}

func (f *fakeGitea) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if time.Now().Before(f.limitTill) {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(f.limitTill).Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "rate limit exceeded"})
		return
	}
	if f.token != "" && r.Header.Get("Authorization") != "token "+f.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "token is required"})
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/api/v1/")
	if !ok || r.Method != http.MethodGet {
		notFound(w)
		return
	}
	seg := strings.Split(p, "/")
	switch {
	case len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos":
		if !f.orgs[strings.ToLower(seg[1])] {
			notFound(w)
			return
		}
		f.ownerRepos(w, r, seg[1])
	case len(seg) == 3 && seg[0] == "users" && seg[2] == "repos":
		if !f.users[strings.ToLower(seg[1])] {
			notFound(w)
			return
		}
		f.ownerRepos(w, r, seg[1])
	case len(seg) >= 3 && seg[0] == "repos":
		repo := f.repo(seg[1], seg[2])
		if repo == nil {
			notFound(w)
			return
		}
		f.repoRoute(w, r, repo, seg[3:])
	default:
		notFound(w)
	}
}

func (f *fakeGitea) repo(owner, name string) *ftRepo {
	for _, r := range f.repos {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

func (f *fakeGitea) repoJSON(r *ftRepo) map[string]any {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	topics := r.Topics
	if topics == nil {
		topics = []string{}
	}
	return map[string]any{
		"id":             r.ID,
		"owner":          map[string]any{"id": 1, "login": r.Owner, "username": r.Owner, "full_name": ""},
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"description":    "",
		"empty":          r.Empty,
		"private":        r.Private,
		"fork":           false,
		"archived":       r.Archived,
		"html_url":       f.srv.URL + "/" + r.Owner + "/" + r.Name,
		"ssh_url":        "git@" + host + ":" + r.Owner + "/" + r.Name + ".git",
		"clone_url":      f.srv.URL + "/" + r.Owner + "/" + r.Name + ".git",
		"default_branch": r.DefaultBranch,
		"topics":         topics,
	}
}

func (f *fakeGitea) ownerRepos(w http.ResponseWriter, r *http.Request, owner string) {
	var out []any
	for _, repo := range f.repos {
		if strings.EqualFold(repo.Owner, owner) {
			out = append(out, f.repoJSON(repo))
		}
	}
	f.paginate(w, r, out)
}

func branchJSON(b ftBranch) map[string]any {
	return map[string]any{"name": b.Name, "protected": false, "commit": map[string]any{"id": b.SHA, "message": "m", "url": "u"}}
}

func (f *fakeGitea) resolve(repo *ftRepo, ref string) (string, bool) {
	if ref == "" {
		ref = repo.DefaultBranch
	}
	for _, b := range repo.Branches {
		if b.Name == ref || b.SHA == ref {
			return b.SHA, true
		}
	}
	if s, ok := repo.Tags[ref]; ok {
		return s, true
	}
	return "", false
}

func (f *fakeGitea) repoRoute(w http.ResponseWriter, r *http.Request, repo *ftRepo, rest []string) {
	q := r.URL.Query()
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, f.repoJSON(repo))
	case len(rest) == 1 && rest[0] == "labels":
		var out []any
		for i, l := range repo.Labels {
			out = append(out, map[string]any{"id": i + 1, "name": l, "color": "ee0701"})
		}
		f.paginate(w, r, out)
	case len(rest) == 1 && rest[0] == "branches":
		var out []any
		for _, b := range repo.Branches {
			out = append(out, branchJSON(b))
		}
		f.paginate(w, r, out)
	case len(rest) >= 2 && rest[0] == "branches":
		name := strings.Join(rest[1:], "/")
		for _, b := range repo.Branches {
			if b.Name == name {
				writeJSON(w, http.StatusOK, branchJSON(b))
				return
			}
		}
		notFound(w)
	case len(rest) >= 2 && rest[0] == "tags":
		name := strings.Join(rest[1:], "/")
		s, ok := repo.Tags[name]
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "id": "tagobject", "commit": map[string]any{"sha": s, "url": "u"}})
	case len(rest) == 3 && rest[0] == "git" && rest[1] == "commits":
		for _, b := range repo.Branches {
			if b.SHA == rest[2] {
				writeJSON(w, http.StatusOK, map[string]any{"sha": b.SHA, "url": "u"})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "object does not exist"})
	case len(rest) >= 1 && rest[0] == "contents":
		sha, ok := f.resolve(repo, q.Get("ref"))
		if !ok {
			notFound(w)
			return
		}
		path := strings.Trim(strings.Join(rest[1:], "/"), "/")
		files := repo.Files[sha]
		if slices.Contains(files, path) {
			writeJSON(w, http.StatusOK, map[string]any{"name": path, "path": path, "type": "file", "content": "", "encoding": "base64"})
			return
		}
		var dir []any
		for _, file := range files {
			if path == "" || strings.HasPrefix(file, path+"/") {
				dir = append(dir, map[string]any{"name": file, "path": file, "type": "file"})
			}
		}
		if len(dir) == 0 {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, dir)
	case len(rest) == 1 && rest[0] == "pulls":
		state := q.Get("state")
		var out []any
		for _, p := range repo.Pulls {
			if state != "all" && p.State != state {
				continue
			}
			labels := []any{}
			for i, l := range p.Labels {
				labels = append(labels, map[string]any{"id": i + 1, "name": l, "color": "ee0701"})
			}
			out = append(out, map[string]any{
				"id": 100 + p.Number, "number": p.Number, "title": p.Title, "state": p.State, "merged": p.Merged, "draft": p.Draft,
				"html_url": fmt.Sprintf("%s/%s/%s/pulls/%d", f.srv.URL, repo.Owner, repo.Name, p.Number),
				"user":     map[string]any{"id": 9, "login": p.User, "username": p.User},
				"labels":   labels,
				"head":     map[string]any{"label": p.Head, "ref": p.Head, "sha": p.SHA},
				"base":     map[string]any{"label": p.Base, "ref": p.Base, "sha": "base"},
			})
		}
		f.paginate(w, r, out)
	default:
		notFound(w)
	}
}

// paginate serves items with Gitea's page/limit pagination headers.
func (f *fakeGitea) paginate(w http.ResponseWriter, r *http.Request, items []any) {
	per := f.maxPer
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < per {
		per = v
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	start := min((page-1)*per, len(items))
	end := min(start+per, len(items))
	w.Header().Set("X-Total-Count", strconv.Itoa(len(items)))
	if end < len(items) && !f.noLink {
		u := *r.URL
		qq := u.Query()
		qq.Set("page", strconv.Itoa(page+1))
		u.RawQuery = qq.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next",<http://%s%s>; rel="first"`, r.Host, u.RequestURI(), r.Host, r.URL.Path))
	}
	out := items[start:end]
	if out == nil {
		out = []any{}
	}
	writeJSON(w, http.StatusOK, out)
}
