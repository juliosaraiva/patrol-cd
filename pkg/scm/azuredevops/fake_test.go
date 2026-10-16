package azuredevops_test

import (
	"encoding/base64"
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

// fakeAzure is an in-process Azure DevOps REST API 7.1 subset: repositories, branch
// stats, refs (continuation tokens), items, commits and project pull requests
// ($top/$skip), PAT basic authentication (a wrong PAT gets the 203 sign-in page, as
// Azure DevOps answers) and rate-limit injection.
type fakeAzure struct {
	srv *httptest.Server

	mu        sync.Mutex
	org       string
	pat       string
	projects  map[string][]*faRepo // project name → repositories
	pulls     map[string][]faPull  // project name → pull requests
	refPage   int
	limitTill time.Time
	requests  []string
}

type faBranch struct{ Name, SHA string }

type faPull struct {
	ID          int64
	Repo, Title string
	From, To    string
	SHA, Author string
	Status      string
	Labels      []string
}

type faRepo struct {
	ID, Name      string
	DefaultBranch string
	Disabled      bool
	Branches      []faBranch
	Tags          map[string]string // tag → commit (annotated: peeled)
	Files         map[string][]string
}

func newFakeAzure(t *testing.T, org string) *fakeAzure {
	t.Helper()
	f := &fakeAzure{org: org, projects: map[string][]*faRepo{}, pulls: map[string][]faPull{}, refPage: 2}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAzure) orgURL() string { return f.srv.URL + "/" + f.org }

func (f *fakeAzure) injectRateLimit(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitTill = time.Now().Add(d)
}

func (f *fakeAzure) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8; api-version=7.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func azError(w http.ResponseWriter, status int, typeKey, msg string) {
	writeJSON(w, status, map[string]any{"$id": "1", "innerException": nil, "message": msg,
		"typeName": "Microsoft.TeamFoundation." + typeKey + ", Microsoft.TeamFoundation", "typeKey": typeKey, "errorCode": 0, "eventId": 3000})
}

func (f *fakeAzure) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if time.Now().Before(f.limitTill) {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(f.limitTill).Seconds())+1))
		w.Header().Set("X-RateLimit-Resource", "Core")
		azError(w, http.StatusTooManyRequests, "RequestThrottledException", "Request was blocked due to exceeding usage of resource 'Core'.")
		return
	}
	switch auth := r.Header.Get("Authorization"); {
	case auth == "":
		azError(w, http.StatusUnauthorized, "UnauthorizedRequestException", "TF400813: The user is not authorized to access this resource.")
		return
	case auth != "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+f.pat)):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		_, _ = w.Write([]byte("<html><head><title>Azure DevOps Services | Sign In</title></head></html>"))
		return
	}
	if r.URL.Query().Get("api-version") != "7.1" {
		azError(w, http.StatusBadRequest, "VssVersionNotSpecifiedException", "No api-version was supplied.")
		return
	}
	seg := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(seg) < 4 || seg[0] != f.org || seg[2] != "_apis" || seg[3] != "git" || r.Method != http.MethodGet {
		azError(w, http.StatusNotFound, "", "not found")
		return
	}
	project := seg[1]
	repos, ok := f.projects[project]
	if !ok {
		azError(w, http.StatusNotFound, "ProjectDoesNotExistWithNameException",
			"TF200016: The following project does not exist: "+project+". Verify that the name of the project is correct and that the project exists on the specified Azure DevOps Server.")
		return
	}
	rest := seg[4:]
	switch {
	case len(rest) == 1 && rest[0] == "repositories":
		var out []any
		for _, repo := range repos {
			out = append(out, f.repoJSON(project, repo))
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
	case len(rest) == 1 && rest[0] == "pullrequests":
		f.pullRequests(w, r, project)
	case len(rest) >= 2 && rest[0] == "repositories":
		var repo *faRepo
		for _, rr := range repos {
			if rr.ID == rest[1] || strings.EqualFold(rr.Name, rest[1]) {
				repo = rr
			}
		}
		if repo == nil || repo.Disabled {
			azError(w, http.StatusNotFound, "GitRepositoryNotFoundException",
				"TF401019: The Git repository with name or identifier "+rest[1]+" does not exist or you do not have permissions for the operation you are attempting.")
			return
		}
		f.repoRoute(w, r, project, repo, rest[2:])
	default:
		azError(w, http.StatusNotFound, "", "not found")
	}
}

func (f *fakeAzure) repoJSON(project string, r *faRepo) map[string]any {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	out := map[string]any{
		"id": r.ID, "name": r.Name, "url": f.orgURL() + "/_apis/git/repositories/" + r.ID,
		"project":    map[string]any{"id": "p-1", "name": project, "state": "wellFormed", "visibility": "private"},
		"remoteUrl":  "https://acme@" + host + "/acme/" + project + "/_git/" + r.Name,
		"sshUrl":     "git@ssh." + host + ":v3/acme/" + project + "/" + r.Name,
		"webUrl":     f.orgURL() + "/" + project + "/_git/" + r.Name,
		"isDisabled": r.Disabled,
		"size":       1024,
	}
	if r.DefaultBranch != "" {
		out["defaultBranch"] = "refs/heads/" + r.DefaultBranch
	}
	return out
}

func statsJSON(b faBranch) map[string]any {
	return map[string]any{"name": b.Name, "aheadCount": 0, "behindCount": 0, "isBaseVersion": false,
		"commit": map[string]any{"commitId": b.SHA, "comment": "c"}}
}

func (f *fakeAzure) resolve(repo *faRepo, version, typ string) (string, bool) {
	switch typ {
	case "", "branch":
		if version == "" {
			version = repo.DefaultBranch
		}
		for _, b := range repo.Branches {
			if b.Name == version {
				return b.SHA, true
			}
		}
	case "commit":
		if _, ok := repo.Files[version]; ok {
			return version, true
		}
	case "tag":
		s, ok := repo.Tags[version]
		return s, ok
	}
	return "", false
}

func (f *fakeAzure) repoRoute(w http.ResponseWriter, r *http.Request, project string, repo *faRepo, rest []string) {
	q := r.URL.Query()
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, f.repoJSON(project, repo))
	case len(rest) == 2 && rest[0] == "stats" && rest[1] == "branches":
		if name := q.Get("name"); name != "" {
			for _, b := range repo.Branches {
				if b.Name == name {
					writeJSON(w, http.StatusOK, statsJSON(b))
					return
				}
			}
			azError(w, http.StatusNotFound, "GitBranchNotFoundException", "TF401175: The version descriptor <Branch: "+name+"> could not be resolved to a version in the repository "+repo.Name)
			return
		}
		var out []any
		for _, b := range repo.Branches {
			out = append(out, statsJSON(b))
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
	case len(rest) == 2 && rest[0] == "commits":
		for _, b := range repo.Branches {
			if b.SHA == rest[1] {
				writeJSON(w, http.StatusOK, map[string]any{"commitId": b.SHA})
				return
			}
		}
		azError(w, http.StatusNotFound, "GitUnresolvableToCommitException", "TF401175: commit not found")
	case len(rest) == 1 && rest[0] == "refs":
		filter := q.Get("filter")
		var all []any
		for _, b := range repo.Branches {
			if strings.HasPrefix("heads/"+b.Name, filter) {
				all = append(all, map[string]any{"name": "refs/heads/" + b.Name, "objectId": b.SHA})
			}
		}
		for t, s := range repo.Tags {
			if strings.HasPrefix("tags/"+t, filter) {
				all = append(all, map[string]any{"name": "refs/tags/" + t, "objectId": "aaaa" + s[4:], "peeledObjectId": s})
			}
		}
		slices.SortFunc(all, func(a, b any) int {
			return strings.Compare(a.(map[string]any)["name"].(string), b.(map[string]any)["name"].(string))
		})
		start, _ := strconv.Atoi(q.Get("continuationToken"))
		end := min(start+f.refPage, len(all))
		if end < len(all) {
			w.Header().Set("X-Ms-Continuationtoken", strconv.Itoa(end))
		}
		page := all[min(start, len(all)):end]
		writeJSON(w, http.StatusOK, map[string]any{"count": len(page), "value": page})
	case len(rest) == 1 && rest[0] == "items":
		if q.Get("$format") != "json" {
			azError(w, http.StatusBadRequest, "", "this fake expects $format=json")
			return
		}
		sha, ok := f.resolve(repo, q.Get("versionDescriptor.version"), q.Get("versionDescriptor.versionType"))
		if !ok {
			azError(w, http.StatusNotFound, "GitUnresolvableToCommitException", "TF401175: version could not be resolved")
			return
		}
		path := strings.Trim(q.Get("path"), "/")
		files := repo.Files[sha]
		switch {
		case slices.Contains(files, path):
			writeJSON(w, http.StatusOK, map[string]any{"objectId": "b1", "gitObjectType": "blob", "path": "/" + path})
		case path == "" || slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, path+"/") }):
			writeJSON(w, http.StatusOK, map[string]any{"objectId": "t1", "gitObjectType": "tree", "path": "/" + path, "isFolder": true})
		default:
			azError(w, http.StatusNotFound, "GitItemNotFoundException", "TF401174: The item '/"+path+"' could not be found in the repository '"+repo.Name+"' at the version specified by '<Branch: "+q.Get("versionDescriptor.version")+">'.")
		}
	default:
		azError(w, http.StatusNotFound, "", "not found")
	}
}

func (f *fakeAzure) pullRequests(w http.ResponseWriter, r *http.Request, project string) {
	q := r.URL.Query()
	status := q.Get("searchCriteria.status")
	if status == "" {
		status = "active"
	}
	var all []any
	for _, p := range f.pulls[project] {
		if status != "all" && p.Status != status {
			continue
		}
		if t := q.Get("searchCriteria.targetRefName"); t != "" && t != "refs/heads/"+p.To {
			continue
		}
		labels := []any{}
		for _, l := range p.Labels {
			labels = append(labels, map[string]any{"id": "l-" + l, "name": l, "active": true})
		}
		all = append(all, map[string]any{
			"repository": map[string]any{"id": "r-" + p.Repo, "name": p.Repo, "webUrl": f.orgURL() + "/" + project + "/_git/" + p.Repo,
				"project": map[string]any{"name": project}},
			"pullRequestId": p.ID, "status": p.Status, "title": p.Title, "isDraft": false,
			"createdBy":             map[string]any{"displayName": strings.ToUpper(p.Author), "uniqueName": p.Author + "@example.com"},
			"sourceRefName":         "refs/heads/" + p.From,
			"targetRefName":         "refs/heads/" + p.To,
			"lastMergeSourceCommit": map[string]any{"commitId": p.SHA},
			"labels":                labels,
			"url":                   fmt.Sprintf("%s/%s/_apis/git/repositories/r-%s/pullRequests/%d", f.orgURL(), project, p.Repo, p.ID),
		})
	}
	top, err := strconv.Atoi(q.Get("$top"))
	if err != nil || top <= 0 {
		top = 101
	}
	skip, _ := strconv.Atoi(q.Get("$skip"))
	s := min(skip, len(all))
	e := min(s+top, len(all))
	page := all[s:e]
	if page == nil {
		page = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(page), "value": page})
}
