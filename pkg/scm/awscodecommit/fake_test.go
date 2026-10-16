package awscodecommit_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	account = "123456789012"
	region  = "eu-west-1"
)

// fakeAWS is an in-process AWS endpoint serving the CodeCommit and Resource Groups
// Tagging JSON 1.1 APIs (dispatched on X-Amz-Target) and STS AssumeRole (query
// protocol). It records the access key of every signed request.
type fakeAWS struct {
	srv *httptest.Server

	mu         sync.Mutex
	maxPer     int
	repos      []*fakeRepo
	throttle   bool
	accessKeys []string
	targets    []string
	tagQueries []json.RawMessage
	assumed    []string
}

type fakeRepo struct {
	Name, ID      string
	DefaultBranch string
	Tags          map[string]string
	Branches      []fakeBranch
	// Files maps commit → paths; directories exist implicitly; entries ending in "@" are
	// submodules and in "->" symbolic links.
	Files map[string][]string
}

type fakeBranch struct{ Name, SHA string }

func newFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()
	f := &fakeAWS{maxPer: 3}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAWS) add(r *fakeRepo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, r)
}

func (f *fakeAWS) setThrottle(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.throttle = on
}

func (f *fakeAWS) snapshot() (keys, targets []string, tagQueries []json.RawMessage, assumed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accessKeys), slices.Clone(f.targets), slices.Clone(f.tagQueries), slices.Clone(f.assumed)
}

func awsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("X-Amzn-Requestid", "req-1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func awsError(w http.ResponseWriter, code, msg string) {
	awsJSON(w, http.StatusBadRequest, map[string]string{"__type": code, "message": msg})
}

// accessKey extracts the access key ID from a SigV4 Authorization header.
func accessKey(auth string) string {
	_, rest, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return ""
	}
	k, _, _ := strings.Cut(rest, "/")
	return k
}

func (f *fakeAWS) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessKeys = append(f.accessKeys, accessKey(r.Header.Get("Authorization")))
	target := r.Header.Get("X-Amz-Target")
	f.targets = append(f.targets, target)
	if target == "" {
		f.sts(w, body)
		return
	}
	if f.throttle {
		awsError(w, "ThrottlingException", "Rate exceeded")
		return
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		awsError(w, "SerializationException", err.Error())
		return
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(in[k], &s)
		return s
	}
	switch target {
	case "CodeCommit_20150413.ListRepositories":
		var out []any
		for _, repo := range f.repos {
			out = append(out, map[string]string{"repositoryName": repo.Name, "repositoryId": repo.ID})
		}
		items, next := f.page(out, str("nextToken"))
		resp := map[string]any{"repositories": items}
		if next != "" {
			resp["nextToken"] = next
		}
		awsJSON(w, http.StatusOK, resp)
	case "CodeCommit_20150413.GetRepository":
		repo := f.repo(str("repositoryName"))
		if repo == nil {
			awsError(w, "RepositoryDoesNotExistException", str("repositoryName")+" does not exist")
			return
		}
		meta := map[string]any{
			"accountId": account, "repositoryId": repo.ID, "repositoryName": repo.Name,
			"cloneUrlHttp": "https://git-codecommit." + region + ".amazonaws.com/v1/repos/" + repo.Name,
			"cloneUrlSsh":  "ssh://git-codecommit." + region + ".amazonaws.com/v1/repos/" + repo.Name,
			"Arn":          "arn:aws:codecommit:" + region + ":" + account + ":" + repo.Name,
		}
		if repo.DefaultBranch != "" {
			meta["defaultBranch"] = repo.DefaultBranch
		}
		awsJSON(w, http.StatusOK, map[string]any{"repositoryMetadata": meta})
	case "CodeCommit_20150413.ListBranches":
		repo := f.repo(str("repositoryName"))
		if repo == nil {
			awsError(w, "RepositoryDoesNotExistException", str("repositoryName")+" does not exist")
			return
		}
		var names []any
		for _, b := range repo.Branches {
			names = append(names, b.Name)
		}
		items, next := f.page(names, str("nextToken"))
		resp := map[string]any{"branches": items}
		if next != "" {
			resp["nextToken"] = next
		}
		awsJSON(w, http.StatusOK, resp)
	case "CodeCommit_20150413.GetBranch":
		repo := f.repo(str("repositoryName"))
		if repo == nil {
			awsError(w, "RepositoryDoesNotExistException", "does not exist")
			return
		}
		for _, b := range repo.Branches {
			if b.Name == str("branchName") {
				awsJSON(w, http.StatusOK, map[string]any{"branch": map[string]string{"branchName": b.Name, "commitId": b.SHA}})
				return
			}
		}
		awsError(w, "BranchDoesNotExistException", "Branch '"+str("branchName")+"' does not exist")
	case "CodeCommit_20150413.GetFolder":
		f.getFolder(w, f.repo(str("repositoryName")), str("commitSpecifier"), str("folderPath"))
	case "ResourceGroupsTaggingAPI_20170126.GetResources":
		f.tagQueries = append(f.tagQueries, slices.Clone(body))
		f.getResources(w, in)
	default:
		awsError(w, "UnknownOperationException", target)
	}
}

func (f *fakeAWS) repo(name string) *fakeRepo {
	for _, r := range f.repos {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func (f *fakeAWS) page(items []any, token string) ([]any, string) {
	start, _ := strconv.Atoi(strings.TrimPrefix(token, "tok-"))
	s := min(start, len(items))
	e := min(s+f.maxPer, len(items))
	next := ""
	if e < len(items) {
		next = "tok-" + strconv.Itoa(e)
	}
	out := items[s:e]
	if out == nil {
		out = []any{}
	}
	return out, next
}

func (f *fakeAWS) getFolder(w http.ResponseWriter, repo *fakeRepo, commit, folder string) {
	if repo == nil {
		awsError(w, "RepositoryDoesNotExistException", "does not exist")
		return
	}
	if commit == "" {
		commit = repo.DefaultBranch
	}
	sha := ""
	for _, b := range repo.Branches {
		if b.Name == commit || b.SHA == commit {
			sha = b.SHA
		}
	}
	files, ok := repo.Files[sha]
	if !ok {
		awsError(w, "CommitDoesNotExistException", "The specified commit does not exist")
		return
	}
	dir := strings.Trim(folder, "/")
	children := map[string]string{} // name → kind
	for _, file := range files {
		kind := "file"
		switch {
		case strings.HasSuffix(file, "@"):
			file, kind = strings.TrimSuffix(file, "@"), "submodule"
		case strings.HasSuffix(file, "->"):
			file, kind = strings.TrimSuffix(file, "->"), "symlink"
		}
		rel := file
		if dir != "" {
			if !strings.HasPrefix(file, dir+"/") {
				continue
			}
			rel = strings.TrimPrefix(file, dir+"/")
		}
		first, rest, nested := strings.Cut(rel, "/")
		if nested && rest != "" {
			children[first] = "folder"
		} else if children[first] == "" {
			children[first] = kind
		}
	}
	if dir != "" && len(children) == 0 {
		awsError(w, "FolderDoesNotExistException", "The specified folder does not exist")
		return
	}
	out := map[string]any{"commitId": sha, "folderPath": "/" + dir, "treeId": "t", "files": []any{}, "subFolders": []any{}, "subModules": []any{}, "symbolicLinks": []any{}}
	for name, kind := range children {
		abs := path.Join("/", dir, name)
		switch kind {
		case "folder":
			out["subFolders"] = append(out["subFolders"].([]any), map[string]string{"absolutePath": abs, "relativePath": name, "treeId": "t"})
		case "submodule":
			out["subModules"] = append(out["subModules"].([]any), map[string]string{"absolutePath": abs, "relativePath": name, "commitId": "c"})
		case "symlink":
			out["symbolicLinks"] = append(out["symbolicLinks"].([]any), map[string]string{"absolutePath": abs, "relativePath": name, "blobId": "b", "fileMode": "SYMLINK"})
		default:
			out["files"] = append(out["files"].([]any), map[string]string{"absolutePath": abs, "relativePath": name, "blobId": "b", "fileMode": "NORMAL"})
		}
	}
	awsJSON(w, http.StatusOK, out)
}

func (f *fakeAWS) getResources(w http.ResponseWriter, in map[string]json.RawMessage) {
	var filters []struct {
		Key    string
		Values []string
	}
	_ = json.Unmarshal(in["TagFilters"], &filters)
	var token string
	_ = json.Unmarshal(in["PaginationToken"], &token)
	var out []any
	for _, repo := range f.repos {
		match := true
		for _, flt := range filters {
			v, ok := repo.Tags[flt.Key]
			if !ok || (len(flt.Values) > 0 && !slices.Contains(flt.Values, v)) {
				match = false
			}
		}
		if !match {
			continue
		}
		var tags []any
		for k, v := range repo.Tags {
			tags = append(tags, map[string]string{"Key": k, "Value": v})
		}
		out = append(out, map[string]any{"ResourceARN": "arn:aws:codecommit:" + region + ":" + account + ":" + repo.Name, "Tags": tags})
	}
	// A malformed ARN is skipped by the provider (upstream logs a warning).
	if len(filters) > 0 && token == "" {
		out = append(out, map[string]any{"ResourceARN": "not-an-arn"})
	}
	items, next := f.page(out, token)
	awsJSON(w, http.StatusOK, map[string]any{"ResourceTagMappingList": items, "PaginationToken": next})
}

// sts answers AssumeRole with temporary credentials whose access key encodes the role.
func (f *fakeAWS) sts(w http.ResponseWriter, body []byte) {
	form, _ := url.ParseQuery(string(body))
	if form.Get("Action") != "AssumeRole" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	role := form.Get("RoleArn")
	f.assumed = append(f.assumed, role)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>ASIAASSUMEDROLE0001</AccessKeyId>
      <SecretAccessKey>assumed-secret</SecretAccessKey>
      <SessionToken>assumed-session-token</SessionToken>
      <Expiration>%s</Expiration>
    </Credentials>
    <AssumedRoleUser>
      <Arn>%s/session</Arn>
      <AssumedRoleId>AROAEXAMPLE:session</AssumedRoleId>
    </AssumedRoleUser>
  </AssumeRoleResult>
  <ResponseMetadata><RequestId>sts-req-1</RequestId></ResponseMetadata>
</AssumeRoleResponse>`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), role)
}
