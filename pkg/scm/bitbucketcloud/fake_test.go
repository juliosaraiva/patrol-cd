package bitbucketcloud_test

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

// fakeBitbucket is an in-process Bitbucket Cloud API 2.0 subset with "next"-link
// pagination, basic or bearer authentication, rate-limit injection (429 without
// Retry-After, as Bitbucket Cloud answers) and a diffstat endpoint.
type fakeBitbucket struct {
	srv *httptest.Server

	mu         sync.Mutex
	auth       string
	maxPer     int
	foreignNxt string // when set, "next" links point to this host
	workspaces map[string]bool
	repos      []*fcRepo
	diffstats  map[string][]map[string]any
	limitTill  time.Time
	requests   []string
}

type fcBranch struct{ Name, SHA string }

type fcPull struct {
	ID          int64
	Title       string
	From, To    string
	SHA, Author string
	State       string
}

type fcRepo struct {
	Workspace, Slug string
	UUID            string
	MainBranch      string
	Private         bool
	Branches        []fcBranch
	Tags            map[string]string
	Files           map[string][]string
	Pulls           []fcPull
}

func newFakeBitbucket(t *testing.T) *fakeBitbucket {
	t.Helper()
	f := &fakeBitbucket{maxPer: 3, workspaces: map[string]bool{}, diffstats: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBitbucket) apiURL() string { return f.srv.URL + "/2.0" }

func (f *fakeBitbucket) add(r *fcRepo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces[r.Workspace] = true
	f.repos = append(f.repos, r)
}

func (f *fakeBitbucket) injectRateLimit(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitTill = time.Now().Add(d)
}

func (f *fakeBitbucket) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bbError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"message": msg}})
}

func (f *fakeBitbucket) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if time.Now().Before(f.limitTill) {
		bbError(w, http.StatusTooManyRequests, "Rate limit for this resource has been exceeded")
		return
	}
	if f.auth != "" && r.Header.Get("Authorization") != f.auth {
		bbError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/2.0/repositories/")
	if !ok || r.Method != http.MethodGet {
		bbError(w, http.StatusNotFound, "Resource not found")
		return
	}
	seg := strings.Split(p, "/")
	if !f.workspaces[seg[0]] {
		bbError(w, http.StatusNotFound, "No workspace with identifier '"+seg[0]+"'.")
		return
	}
	if len(seg) == 1 {
		if r.URL.Query().Get("role") != "member" {
			bbError(w, http.StatusBadRequest, "role=member expected by this fake")
			return
		}
		var out []any
		for _, repo := range f.repos {
			if repo.Workspace == seg[0] {
				out = append(out, f.repoJSON(repo))
			}
		}
		f.page(w, r, out)
		return
	}
	var repo *fcRepo
	for _, rr := range f.repos {
		if rr.Workspace == seg[0] && rr.Slug == seg[1] {
			repo = rr
		}
	}
	if repo == nil {
		bbError(w, http.StatusNotFound, "Repository "+seg[0]+"/"+seg[1]+" not found")
		return
	}
	f.repoRoute(w, r, repo, seg[2:])
}

func (f *fakeBitbucket) repoJSON(r *fcRepo) map[string]any {
	full := r.Workspace + "/" + r.Slug
	var main any
	if r.MainBranch != "" {
		main = map[string]any{"name": r.MainBranch, "type": "branch"}
	}
	return map[string]any{
		"type": "repository", "uuid": r.UUID, "slug": r.Slug, "name": strings.ToUpper(r.Slug[:1]) + r.Slug[1:], "full_name": full,
		"is_private": r.Private, "scm": "git", "mainbranch": main,
		"workspace": map[string]any{"slug": r.Workspace, "type": "workspace"},
		"links": map[string]any{
			"self": map[string]any{"href": f.apiURL() + "/repositories/" + full},
			"html": map[string]any{"href": "https://bitbucket.org/" + full},
			"clone": []any{
				map[string]any{"name": "https", "href": "https://x-token-auth@bitbucket.org/" + full + ".git"},
				map[string]any{"name": "ssh", "href": "git@bitbucket.org:" + full + ".git"},
			},
		},
	}
}

func refJSON(typ, name, sha string) map[string]any {
	return map[string]any{"type": typ, "name": name, "target": map[string]any{"type": "commit", "hash": sha}}
}

func (f *fakeBitbucket) repoRoute(w http.ResponseWriter, r *http.Request, repo *fcRepo, rest []string) {
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, f.repoJSON(repo))
	case len(rest) == 2 && rest[0] == "refs" && rest[1] == "branches":
		var out []any
		for _, b := range repo.Branches {
			out = append(out, refJSON("branch", b.Name, b.SHA))
		}
		f.page(w, r, out)
	case len(rest) >= 3 && rest[0] == "refs" && rest[1] == "branches":
		name := strings.Join(rest[2:], "/")
		for _, b := range repo.Branches {
			if b.Name == name {
				writeJSON(w, http.StatusOK, refJSON("branch", b.Name, b.SHA))
				return
			}
		}
		bbError(w, http.StatusNotFound, "Branch not found")
	case len(rest) >= 3 && rest[0] == "refs" && rest[1] == "tags":
		name := strings.Join(rest[2:], "/")
		if s, ok := repo.Tags[name]; ok {
			writeJSON(w, http.StatusOK, refJSON("tag", name, s))
			return
		}
		bbError(w, http.StatusNotFound, "Tag not found")
	case len(rest) == 2 && rest[0] == "commit":
		for _, b := range repo.Branches {
			if strings.HasPrefix(b.SHA, rest[1]) {
				writeJSON(w, http.StatusOK, map[string]any{"type": "commit", "hash": b.SHA})
				return
			}
		}
		bbError(w, http.StatusNotFound, "Commit not found")
	case len(rest) >= 2 && rest[0] == "src":
		if r.URL.Query().Get("format") != "meta" {
			bbError(w, http.StatusBadRequest, "format=meta expected by this fake")
			return
		}
		if len(rest[1]) != 40 {
			bbError(w, http.StatusNotFound, "src expects a commit")
			return
		}
		path := strings.Trim(strings.Join(rest[2:], "/"), "/")
		files, ok := repo.Files[rest[1]]
		switch {
		case !ok:
			bbError(w, http.StatusNotFound, "Commit not found")
		case slices.Contains(files, path):
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "type": "commit_file"})
		case path == "" || slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, path+"/") }):
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "type": "commit_directory"})
		default:
			bbError(w, http.StatusNotFound, "No such file or directory: "+path)
		}
	case len(rest) == 1 && rest[0] == "pullrequests":
		states := r.URL.Query()["state"]
		var out []any
		for _, p := range repo.Pulls {
			if !slices.Contains(states, p.State) {
				continue
			}
			out = append(out, map[string]any{
				"type": "pullrequest", "id": p.ID, "title": p.Title, "state": p.State, "draft": false,
				"source":      map[string]any{"branch": map[string]any{"name": p.From}, "commit": map[string]any{"hash": p.SHA[:12]}},
				"destination": map[string]any{"branch": map[string]any{"name": p.To}, "commit": map[string]any{"hash": "000000000001"}},
				"author":      map[string]any{"type": "user", "nickname": p.Author, "display_name": strings.ToUpper(p.Author)},
				"links":       map[string]any{"html": map[string]any{"href": fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/%d", repo.Workspace, repo.Slug, p.ID)}},
			})
		}
		f.page(w, r, out)
	case len(rest) == 2 && rest[0] == "diffstat":
		if r.URL.Query().Get("renames") != "true" {
			bbError(w, http.StatusBadRequest, "renames expected")
			return
		}
		ds, ok := f.diffstats[rest[1]]
		if !ok {
			bbError(w, http.StatusNotFound, "Commit not found")
			return
		}
		var out []any
		for _, d := range ds {
			out = append(out, d)
		}
		f.page(w, r, out)
	default:
		bbError(w, http.StatusNotFound, "Resource not found")
	}
}

func (f *fakeBitbucket) page(w http.ResponseWriter, r *http.Request, items []any) {
	per := f.maxPer
	if v, err := strconv.Atoi(r.URL.Query().Get("pagelen")); err == nil && v > 0 && v < per {
		per = v
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	s := min((page-1)*per, len(items))
	e := min(s+per, len(items))
	out := items[s:e]
	if out == nil {
		out = []any{}
	}
	body := map[string]any{"pagelen": per, "page": page, "size": len(items), "values": out}
	if e < len(items) {
		u := *r.URL
		q := u.Query()
		q.Set("page", strconv.Itoa(page+1))
		u.RawQuery = q.Encode()
		host := "http://" + r.Host
		if f.foreignNxt != "" {
			host = f.foreignNxt
		}
		body["next"] = host + u.RequestURI()
	}
	writeJSON(w, http.StatusOK, body)
}
