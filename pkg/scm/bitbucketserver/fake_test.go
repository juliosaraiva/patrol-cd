package bitbucketserver_test

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

// fakeBBS is an in-process Bitbucket Server REST API 1.0 subset with start/limit paging
// (isLastPage/nextPageStart), basic or bearer authentication and rate-limit injection.
type fakeBBS struct {
	srv *httptest.Server

	mu        sync.Mutex
	auth      string // expected Authorization header ("" = anonymous allowed)
	maxPer    int
	projects  map[string]bool
	repos     []*fbRepo
	limitTill time.Time
	requests  []string
	headers   []http.Header
}

type fbBranch struct{ Name, SHA string }

type fbPull struct {
	ID          int64
	Title       string
	From, To    string
	SHA, Author string
	State       string
}

type fbRepo struct {
	ID            int64
	Project, Slug string
	Name          string
	DefaultBranch string
	Archived      bool
	Public        bool
	Branches      []fbBranch
	Tags          map[string]string
	Files         map[string][]string
	Pulls         []fbPull
}

func newFakeBBS(t *testing.T) *fakeBBS {
	t.Helper()
	f := &fakeBBS{maxPer: 3, projects: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBBS) add(r *fbRepo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[strings.ToUpper(r.Project)] = true
	f.repos = append(f.repos, r)
}

func (f *fakeBBS) injectRateLimit(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitTill = time.Now().Add(d)
}

func (f *fakeBBS) log() ([]string, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests), slices.Clone(f.headers)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bbsError(w http.ResponseWriter, status int, exception, msg string) {
	writeJSON(w, status, map[string]any{"errors": []any{map[string]any{"context": nil, "message": msg, "exceptionName": exception}}})
}

func (f *fakeBBS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.headers = append(f.headers, r.Header.Clone())
	if time.Now().Before(f.limitTill) {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(f.limitTill).Seconds())+1))
		bbsError(w, http.StatusTooManyRequests, "com.atlassian.bitbucket.ratelimit.RateLimitedException", "rate limit exceeded")
		return
	}
	if f.auth != "" && r.Header.Get("Authorization") != f.auth {
		bbsError(w, http.StatusUnauthorized, "com.atlassian.bitbucket.auth.IncorrectPasswordAuthenticationException", "Authentication failed. Please check your credentials and try again.")
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/rest/api/1.0/")
	if !ok || r.Method != http.MethodGet {
		bbsError(w, http.StatusNotFound, "", "not found")
		return
	}
	seg := strings.Split(p, "/")
	if len(seg) < 3 || seg[0] != "projects" || seg[2] != "repos" {
		bbsError(w, http.StatusNotFound, "", "not found")
		return
	}
	if !f.projects[strings.ToUpper(seg[1])] {
		bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.project.NoSuchProjectException", fmt.Sprintf("Project %s does not exist.", seg[1]))
		return
	}
	if len(seg) == 3 {
		var out []any
		for _, repo := range f.repos {
			if strings.EqualFold(repo.Project, seg[1]) {
				out = append(out, f.repoJSON(repo))
			}
		}
		f.page(w, r, out)
		return
	}
	var repo *fbRepo
	for _, rr := range f.repos {
		if strings.EqualFold(rr.Project, seg[1]) && strings.EqualFold(rr.Slug, seg[3]) {
			repo = rr
		}
	}
	if repo == nil {
		bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.repository.NoSuchRepositoryException", fmt.Sprintf("Repository %s/%s does not exist.", seg[1], seg[3]))
		return
	}
	f.repoRoute(w, r, repo, seg[4:])
}

func (f *fakeBBS) repoJSON(r *fbRepo) map[string]any {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	name := r.Name
	if name == "" {
		name = r.Slug
	}
	// Personal projects ("~" + user slug) have type PERSONAL and a /users/ web link.
	projType, self := "NORMAL", f.srv.URL+"/projects/"+strings.ToUpper(r.Project)+"/repos/"+r.Slug+"/browse"
	if user, ok := strings.CutPrefix(r.Project, "~"); ok {
		projType, self = "PERSONAL", f.srv.URL+"/users/"+strings.ToLower(user)+"/repos/"+r.Slug+"/browse"
	}
	return map[string]any{
		"slug": r.Slug, "id": r.ID, "name": name, "hierarchyId": "abc", "scmId": "git", "state": "AVAILABLE",
		"statusMessage": "Available", "forkable": true, "archived": r.Archived, "public": r.Public,
		"project": map[string]any{"key": strings.ToUpper(r.Project), "id": 1, "name": r.Project, "public": false, "type": projType},
		"links": map[string]any{
			"clone": []any{
				map[string]any{"href": "ssh://git@" + host + "/" + strings.ToLower(r.Project) + "/" + r.Slug + ".git", "name": "ssh"},
				map[string]any{"href": f.srv.URL + "/scm/" + strings.ToLower(r.Project) + "/" + r.Slug + ".git", "name": "http"},
			},
			"self": []any{map[string]any{"href": self}},
		},
	}
}

func branchJSON(b fbBranch, def bool) map[string]any {
	return map[string]any{"id": "refs/heads/" + b.Name, "displayId": b.Name, "type": "BRANCH", "latestCommit": b.SHA, "latestChangeset": b.SHA, "isDefault": def}
}

func (f *fakeBBS) resolve(r *fbRepo, ref string) (string, bool) {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	if ref == "" {
		ref = r.DefaultBranch
	}
	for _, b := range r.Branches {
		if b.Name == ref || b.SHA == ref {
			return b.SHA, true
		}
	}
	s, ok := r.Tags[strings.TrimPrefix(ref, "refs/tags/")]
	return s, ok
}

func (f *fakeBBS) repoRoute(w http.ResponseWriter, r *http.Request, repo *fbRepo, rest []string) {
	q := r.URL.Query()
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, f.repoJSON(repo))
	case len(rest) == 2 && rest[0] == "branches" && rest[1] == "default":
		if len(repo.Branches) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for _, b := range repo.Branches {
			if b.Name == repo.DefaultBranch {
				writeJSON(w, http.StatusOK, branchJSON(b, true))
				return
			}
		}
		bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.repository.NoSuchBranchException", "Repository has no default branch")
	case len(rest) == 1 && rest[0] == "branches":
		var out []any
		for _, b := range repo.Branches {
			out = append(out, branchJSON(b, b.Name == repo.DefaultBranch))
		}
		f.page(w, r, out)
	case len(rest) == 1 && rest[0] == "commits":
		sha, ok := f.resolve(repo, q.Get("until"))
		if !ok {
			bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.commit.NoSuchCommitException", fmt.Sprintf("Commit '%s' does not exist in repository '%s'.", q.Get("until"), repo.Slug))
			return
		}
		f.page(w, r, []any{map[string]any{"id": sha, "displayId": sha[:11]}})
	case len(rest) >= 1 && rest[0] == "browse":
		sha, ok := f.resolve(repo, q.Get("at"))
		if !ok {
			bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.commit.NoSuchCommitException", "no such revision")
			return
		}
		path := strings.Join(rest[1:], "/")
		files := repo.Files[sha]
		switch {
		case slices.Contains(files, path):
			writeJSON(w, http.StatusOK, map[string]any{"type": "FILE"})
		case path == "" || slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, path+"/") }):
			writeJSON(w, http.StatusOK, map[string]any{"type": "DIRECTORY"})
		default:
			bbsError(w, http.StatusNotFound, "com.atlassian.bitbucket.content.NoSuchPathException", fmt.Sprintf("The path %q does not exist at revision %q", path, q.Get("at")))
		}
	case len(rest) == 1 && rest[0] == "pull-requests":
		state := q.Get("state")
		var out []any
		for _, p := range repo.Pulls {
			if state != "ALL" && p.State != state {
				continue
			}
			if at := q.Get("at"); at != "" && "refs/heads/"+p.To != at {
				continue
			}
			repoJ := f.repoJSON(repo)
			out = append(out, map[string]any{
				"id": p.ID, "version": 0, "title": p.Title, "state": p.State, "open": p.State == "OPEN", "closed": p.State != "OPEN", "draft": false,
				"fromRef": map[string]any{"id": "refs/heads/" + p.From, "displayId": p.From, "latestCommit": p.SHA, "repository": repoJ},
				"toRef":   map[string]any{"id": "refs/heads/" + p.To, "displayId": p.To, "latestCommit": "0000000000000000000000000000000000000001", "repository": repoJ},
				"author":  map[string]any{"user": map[string]any{"name": p.Author, "slug": p.Author, "displayName": strings.ToUpper(p.Author)}, "role": "AUTHOR", "approved": false},
				"links":   map[string]any{"self": []any{map[string]any{"href": fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", f.srv.URL, repo.Project, repo.Slug, p.ID)}}},
			})
		}
		f.page(w, r, out)
	default:
		bbsError(w, http.StatusNotFound, "", "not found")
	}
}

func (f *fakeBBS) page(w http.ResponseWriter, r *http.Request, items []any) {
	limit := f.maxPer
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < limit {
		limit = v
	}
	start := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("start")); err == nil && v > 0 {
		start = v
	}
	s := min(start, len(items))
	e := min(s+limit, len(items))
	out := items[s:e]
	if out == nil {
		out = []any{}
	}
	body := map[string]any{"size": len(out), "limit": limit, "start": start, "isLastPage": e >= len(items), "values": out}
	if e < len(items) {
		body["nextPageStart"] = e
	}
	writeJSON(w, http.StatusOK, body)
}
