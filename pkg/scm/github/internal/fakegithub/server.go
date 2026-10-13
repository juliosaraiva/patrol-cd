// Package fakegithub is a hermetic, in-process GitHub REST API double used by the GitHub
// provider tests. It serves realistic JSON shapes, Link-header pagination, rate-limit
// headers, an optional GitHub Enterprise path prefix (/api/v3), GitHub App JWT
// verification (RS256) and installation tokens with expiry, and records requests
// (without credentials) for assertions.
package fakegithub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Repo is a seeded repository.
type Repo struct {
	Owner         string
	Name          string
	ID            int64
	DefaultBranch string
	Private       bool
	Archived      bool
	Fork          bool
	Topics        []string
	// Branches in API order.
	Branches []Branch
	Tags     map[string]string
	// Files maps commit SHA → file paths.
	Files map[string][]string
	Pulls []Pull
}

// Branch is a branch head.
type Branch struct {
	Name      string
	SHA       string
	Protected bool
}

// Pull is a seeded pull request.
type Pull struct {
	Number   int
	Title    string
	Head     string
	HeadSHA  string
	Base     string
	User     string
	State    string // open (default) or closed
	Labels   []string
	Draft    bool
	MergedAt string
}

// Installation is a seeded App installation.
type Installation struct {
	ID          int64
	Account     string
	AccountType string // Organization or User
}

// Status is a recorded commit status.
type Status struct {
	State, Context, Description, TargetURL string
}

// CheckRun is a recorded check run.
type CheckRun struct {
	ID          int64
	AppID       int64
	HeadSHA     string
	Name        string
	ExternalID  string
	Status      string
	Conclusion  string
	DetailsURL  string
	Title       string
	Summary     string
	CompletedAt string
}

// Request is a recorded request (credentials are never recorded).
type Request struct {
	Method   string
	Path     string // without the enterprise prefix
	RawQuery string
	Auth     string // none, pat, installation, jwt, invalid
}

type issuedToken struct {
	installationID int64
	expires        time.Time
}

type fault struct {
	status    int
	header    http.Header
	body      string
	remaining int
}

// Server is the fake API.
type Server struct {
	*httptest.Server
	// Prefix is "" for github.com-style URLs or "/api/v3" for GitHub Enterprise.
	Prefix string

	mu            sync.Mutex
	owners        map[string]string // lower login → account type
	ownerLogin    map[string]string // lower login → login
	repos         map[string]*Repo  // lower full name
	order         []string
	appID         int64
	appSlug       string
	appKey        *rsa.PublicKey
	installations []Installation
	pats          map[string]bool
	tokens        map[string]issuedToken
	tokenSeq      int
	tokenTTL      time.Duration
	maxPerPage    int
	statuses      map[string][]Status
	checkRuns     []*CheckRun
	checkSeq      int64
	faults        []*fault
	requests      []Request
	remaining     int
	rejectTokens  bool
	statusLimit   int
	now           func() time.Time
}

// New starts a server. prefix is "" or "/api/v3".
func New(prefix string) *Server {
	s := &Server{
		Prefix:     prefix,
		owners:     map[string]string{},
		ownerLogin: map[string]string{},
		repos:      map[string]*Repo{},
		pats:       map[string]bool{},
		tokens:     map[string]issuedToken{},
		tokenTTL:   time.Hour,
		maxPerPage: 100,
		statuses:   map[string][]Status{},
		remaining:  5000,
		now:        time.Now,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// APIURL returns the base URL to configure providers with.
func (s *Server) APIURL() string { return s.URL + s.Prefix }

// SetMaxPerPage caps per_page so small fixtures paginate.
func (s *Server) SetMaxPerPage(n int) { s.mu.Lock(); s.maxPerPage = n; s.mu.Unlock() }

// SetTokenTTL sets the lifetime of minted installation tokens.
func (s *Server) SetTokenTTL(d time.Duration) { s.mu.Lock(); s.tokenTTL = d; s.mu.Unlock() }

// AddOwner declares an account ("Organization" or "User").
func (s *Server) AddOwner(login, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[strings.ToLower(login)] = typ
	s.ownerLogin[strings.ToLower(login)] = login
}

// AddRepo seeds a repository (the owner must have been added).
func (s *Server) AddRepo(r Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(r.Owner + "/" + r.Name)
	c := r
	if _, ok := s.repos[k]; !ok {
		s.order = append(s.order, k)
	}
	s.repos[k] = &c
}

// AddPAT registers a valid personal access token.
func (s *Server) AddPAT(tok string) { s.mu.Lock(); s.pats[tok] = true; s.mu.Unlock() }

// SetApp configures the GitHub App whose JWTs are accepted.
func (s *Server) SetApp(id int64, slug string, pub *rsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appID, s.appSlug, s.appKey = id, slug, pub
}

// AddInstallation seeds an installation of the App.
func (s *Server) AddInstallation(i Installation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installations = append(s.installations, i)
}

// RevokeTokens invalidates every issued installation token.
func (s *Server) RevokeTokens() { s.mu.Lock(); s.tokens = map[string]issuedToken{}; s.mu.Unlock() }

// RejectInstallationTokens makes every installation token (including newly minted ones)
// fail authentication with 401 while reject is true.
func (s *Server) RejectInstallationTokens(reject bool) {
	s.mu.Lock()
	s.rejectTokens = reject
	s.mu.Unlock()
}

// InjectRateLimit makes the next n requests (n <= 0: all) fail with a primary rate-limit
// 403 advertising a reset after d.
func (s *Server) InjectRateLimit(d time.Duration, n int) {
	reset := s.now().Add(d).Unix()
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Used", "5000")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	h.Set("X-RateLimit-Resource", "core")
	s.addFault(&fault{status: http.StatusForbidden, header: h, remaining: n,
		body: `{"message":"API rate limit exceeded for installation ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`})
}

// InjectSecondaryRateLimit makes the next n requests fail with a secondary rate limit
// 403 carrying Retry-After.
func (s *Server) InjectSecondaryRateLimit(retryAfter time.Duration, n int) {
	h := http.Header{}
	h.Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
	s.addFault(&fault{status: http.StatusForbidden, header: h, remaining: n,
		body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/free-pro-team@latest/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`})
}

// FailNext makes the next n requests fail with status and a GitHub-style error body.
func (s *Server) FailNext(n, status int) {
	s.addFault(&fault{status: status, header: http.Header{}, remaining: n,
		body: fmt.Sprintf(`{"message":%q}`, http.StatusText(status))})
}

// SetStatusLimit makes POST /statuses fail with GitHub's 422 "maximum number of
// statuses" error once n statuses exist for a (SHA, context); 0 disables the limit.
func (s *Server) SetStatusLimit(n int) { s.mu.Lock(); s.statusLimit = n; s.mu.Unlock() }

// ClearFaults removes injected faults.
func (s *Server) ClearFaults() { s.mu.Lock(); s.faults = nil; s.mu.Unlock() }

func (s *Server) addFault(f *fault) { s.mu.Lock(); s.faults = append(s.faults, f); s.mu.Unlock() }

// Requests returns the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// CountRequests counts recorded requests with the given method and path prefix.
func (s *Server) CountRequests(method, pathPrefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

// Statuses returns statuses recorded for owner/name@sha.
func (s *Server) Statuses(owner, name, sha string) []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses[strings.ToLower(owner+"/"+name)+"@"+sha])
}

// CheckRuns returns recorded check runs for sha.
func (s *Server) CheckRuns(sha string) []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckRun
	for _, c := range s.checkRuns {
		if c.HeadSHA == sha {
			out = append(out, *c)
		}
	}
	return out
}

// GenerateKey returns an RSA key and its PKCS#1 PEM encoding.
func GenerateKey(bits int) (*rsa.PrivateKey, []byte, error) {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, err
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
}

type authInfo struct {
	kind           string
	installationID int64
}

func (s *Server) authenticate(r *http.Request) authInfo {
	h := r.Header.Get("Authorization")
	if h == "" {
		return authInfo{kind: "none"}
	}
	var tok string
	switch {
	case strings.HasPrefix(h, "Bearer "):
		tok = strings.TrimPrefix(h, "Bearer ")
	case strings.HasPrefix(h, "token "):
		tok = strings.TrimPrefix(h, "token ")
	default:
		return authInfo{kind: "invalid"}
	}
	if strings.Count(tok, ".") == 2 && s.appKey != nil {
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) { return s.appKey, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuedAt(), jwt.WithExpirationRequired(), jwt.WithLeeway(time.Minute))
		if err != nil || claims.Issuer != strconv.FormatInt(s.appID, 10) {
			return authInfo{kind: "invalid"}
		}
		if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 10*time.Minute {
			return authInfo{kind: "invalid"}
		}
		return authInfo{kind: "jwt"}
	}
	if it, ok := s.tokens[tok]; ok {
		if s.now().Before(it.expires) && !s.rejectTokens {
			return authInfo{kind: "installation", installationID: it.installationID}
		}
		return authInfo{kind: "invalid"}
	}
	if s.pats[tok] {
		return authInfo{kind: "pat"}
	}
	return authInfo{kind: "invalid"}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) errorJSON(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"message": msg, "documentation_url": "https://docs.github.com/rest"})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	if s.Prefix != "" {
		if !strings.HasPrefix(path, s.Prefix+"/") {
			s.errorJSON(w, http.StatusNotFound, "Not Found (missing enterprise prefix)")
			return
		}
		path = strings.TrimPrefix(path, s.Prefix)
	}
	auth := s.authenticate(r)
	s.requests = append(s.requests, Request{Method: r.Method, Path: path, RawQuery: r.URL.RawQuery, Auth: auth.kind})

	if s.remaining > 0 {
		s.remaining--
	}
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(s.remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.now().Add(time.Hour).Unix(), 10))
	w.Header().Set("X-RateLimit-Resource", "core")
	w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("FAKE:%d", len(s.requests)))

	if len(s.faults) > 0 {
		f := s.faults[0]
		if f.remaining > 0 {
			f.remaining--
			if f.remaining == 0 {
				s.faults = s.faults[1:]
			}
		}
		for k, v := range f.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	if auth.kind == "invalid" {
		s.errorJSON(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(segs) >= 1 && segs[0] == "app":
		s.serveApp(w, r, auth, segs)
	case len(segs) == 3 && (segs[0] == "orgs" || segs[0] == "users") && segs[2] == "installation":
		s.serveFindInstallation(w, r, auth, segs[0], segs[1])
	case auth.kind == "jwt":
		// App JWTs only authenticate /app endpoints.
		s.errorJSON(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
	case len(segs) == 3 && segs[0] == "orgs" && segs[2] == "repos" && r.Method == http.MethodGet:
		s.serveOwnerRepos(w, r, auth, segs[1], "Organization")
	case len(segs) == 3 && segs[0] == "users" && segs[2] == "repos" && r.Method == http.MethodGet:
		s.serveOwnerRepos(w, r, auth, segs[1], "User")
	case len(segs) == 2 && segs[0] == "users" && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]any{"login": segs[1], "id": 4242, "type": "Bot"})
	case len(segs) >= 3 && segs[0] == "repos":
		s.serveRepo(w, r, auth, segs[1], segs[2], segs[3:])
	default:
		s.errorJSON(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) baseURL(r *http.Request) string {
	return "http://" + r.Host + s.Prefix
}

func (s *Server) repoJSON(r *http.Request, repo *Repo) map[string]any {
	full := repo.Owner + "/" + repo.Name
	typ := s.owners[strings.ToLower(repo.Owner)]
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	topics := repo.Topics
	if topics == nil {
		topics = []string{}
	}
	m := map[string]any{
		"id":             repo.ID,
		"node_id":        fmt.Sprintf("R_kgDO%08d", repo.ID),
		"name":           repo.Name,
		"full_name":      full,
		"private":        repo.Private,
		"owner":          map[string]any{"login": repo.Owner, "id": 1, "type": typ, "site_admin": false},
		"html_url":       "https://github.example/" + full,
		"description":    nil,
		"fork":           repo.Fork,
		"url":            s.baseURL(r) + "/repos/" + full,
		"clone_url":      "https://github.example/" + full + ".git",
		"git_url":        "git://github.example/" + full + ".git",
		"ssh_url":        "git@github.example:" + full + ".git",
		"archived":       repo.Archived,
		"disabled":       false,
		"visibility":     visibility,
		"topics":         topics,
		"default_branch": repo.DefaultBranch,
		"created_at":     "2024-01-02T03:04:05Z",
		"pushed_at":      "2024-01-02T03:04:05Z",
	}
	return m
}

func (s *Server) visible(auth authInfo, repo *Repo) bool {
	if !repo.Private {
		return true
	}
	return auth.kind == "pat" || auth.kind == "installation"
}

// paginate writes items with Link headers.
func paginate[T any](s *Server, w http.ResponseWriter, r *http.Request, items []T) []T {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	if per > s.maxPerPage {
		per = s.maxPerPage
	}
	last := (len(items) + per - 1) / per
	if last == 0 {
		last = 1
	}
	link := func(p int, rel string) string {
		qq := r.URL.Query()
		qq.Set("page", strconv.Itoa(p))
		qq.Set("per_page", strconv.Itoa(per))
		u := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: qq.Encode()}
		return fmt.Sprintf("<%s>; rel=%q", u.String(), rel)
	}
	var links []string
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"), link(page-1, "prev"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	start := (page - 1) * per
	if start >= len(items) {
		return []T{}
	}
	return items[start:min(start+per, len(items))]
}

func (s *Server) serveOwnerRepos(w http.ResponseWriter, r *http.Request, auth authInfo, owner, wantType string) {
	typ, ok := s.owners[strings.ToLower(owner)]
	if !ok || typ != wantType {
		s.errorJSON(w, http.StatusNotFound, "Not Found")
		return
	}
	var out []map[string]any
	for _, k := range s.order {
		repo := s.repos[k]
		if strings.EqualFold(repo.Owner, owner) && s.visible(auth, repo) {
			out = append(out, s.repoJSON(r, repo))
		}
	}
	s.writeJSON(w, http.StatusOK, paginate(s, w, r, out))
}

func (s *Server) resolve(repo *Repo, ref string) (string, bool) {
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "heads/"), "tags/")
	if ref == "" || ref == "HEAD" {
		ref = repo.DefaultBranch
	}
	for _, b := range repo.Branches {
		if b.Name == ref {
			return b.SHA, true
		}
	}
	if sha, ok := repo.Tags[ref]; ok {
		return sha, true
	}
	if _, ok := repo.Files[ref]; ok {
		return ref, true
	}
	for _, b := range repo.Branches {
		if b.SHA == ref {
			return ref, true
		}
	}
	return "", false
}

func (s *Server) serveRepo(w http.ResponseWriter, r *http.Request, auth authInfo, owner, name string, rest []string) {
	repo, ok := s.repos[strings.ToLower(owner+"/"+name)]
	if !ok || !s.visible(auth, repo) {
		s.errorJSON(w, http.StatusNotFound, "Not Found")
		return
	}
	write := r.Method == http.MethodPost || r.Method == http.MethodPatch
	if write && auth.kind == "none" {
		s.errorJSON(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, s.repoJSON(r, repo))
	case len(rest) == 1 && rest[0] == "branches" && r.Method == http.MethodGet:
		var out []map[string]any
		for _, b := range repo.Branches {
			out = append(out, map[string]any{"name": b.Name, "commit": map[string]any{"sha": b.SHA, "url": s.baseURL(r) + "/repos/" + owner + "/" + name + "/commits/" + b.SHA}, "protected": b.Protected})
		}
		if out == nil {
			out = []map[string]any{}
		}
		s.writeJSON(w, http.StatusOK, paginate(s, w, r, out))
	case len(rest) >= 2 && rest[0] == "branches" && r.Method == http.MethodGet:
		bn, _ := url.PathUnescape(strings.Join(rest[1:], "/"))
		for _, b := range repo.Branches {
			if b.Name == bn {
				s.writeJSON(w, http.StatusOK, map[string]any{"name": b.Name, "commit": map[string]any{"sha": b.SHA, "commit": map[string]any{"message": "seed"}}, "protected": b.Protected, "_links": map[string]any{}})
				return
			}
		}
		s.errorJSON(w, http.StatusNotFound, "Branch not found")
	case len(rest) >= 3 && rest[0] == "commits" && rest[len(rest)-1] == "check-runs" && r.Method == http.MethodGet:
		ref := strings.Join(rest[1:len(rest)-1], "/")
		sha, ok := s.resolve(repo, ref)
		if !ok {
			s.errorJSON(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+ref)
			return
		}
		q := r.URL.Query()
		var runs []map[string]any
		for _, c := range s.checkRuns {
			if c.HeadSHA != sha {
				continue
			}
			if n := q.Get("check_name"); n != "" && n != c.Name {
				continue
			}
			if a := q.Get("app_id"); a != "" && a != strconv.FormatInt(c.AppID, 10) {
				continue
			}
			runs = append(runs, checkRunJSON(c))
		}
		if runs == nil {
			runs = []map[string]any{}
		}
		total := len(runs)
		page := paginate(s, w, r, runs)
		s.writeJSON(w, http.StatusOK, map[string]any{"total_count": total, "check_runs": page})
	case len(rest) >= 2 && rest[0] == "commits" && r.Method == http.MethodGet:
		ref, _ := url.PathUnescape(strings.Join(rest[1:], "/"))
		sha, ok := s.resolve(repo, ref)
		if !ok {
			s.errorJSON(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+ref)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.sha") {
			w.Header().Set("Content-Type", "application/vnd.github.v3.sha; charset=utf-8")
			w.Header().Set("ETag", `"`+sha+`"`)
			_, _ = io.WriteString(w, sha)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"sha": sha})
	case len(rest) >= 1 && rest[0] == "contents" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		p := strings.Trim(strings.Join(rest[1:], "/"), "/")
		p, _ = url.PathUnescape(p)
		sha, ok := s.resolve(repo, r.URL.Query().Get("ref"))
		if !ok {
			s.errorJSON(w, http.StatusNotFound, "No commit found for the ref "+r.URL.Query().Get("ref"))
			return
		}
		found, isDir := p == "", p == ""
		for _, f := range repo.Files[sha] {
			if f == p {
				found = true
			} else if strings.HasPrefix(f, p+"/") {
				found, isDir = true, true
			}
		}
		if !found {
			s.errorJSON(w, http.StatusNotFound, "Not Found")
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			return
		}
		if isDir {
			s.writeJSON(w, http.StatusOK, []map[string]any{{"type": "file", "name": "x", "path": p + "/x"}})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"type": "file", "encoding": "base64", "size": 0, "name": p, "path": p, "content": "", "sha": sha})
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodGet:
		q := r.URL.Query()
		state := q.Get("state")
		if state == "" {
			state = "open"
		}
		var out []map[string]any
		for _, pr := range repo.Pulls {
			st := pr.State
			if st == "" {
				st = "open"
			}
			if state != "all" && st != state {
				continue
			}
			if b := q.Get("base"); b != "" && b != pr.Base {
				continue
			}
			out = append(out, s.pullJSON(r, repo, pr))
		}
		if out == nil {
			out = []map[string]any{}
		}
		s.writeJSON(w, http.StatusOK, paginate(s, w, r, out))
	case len(rest) == 2 && rest[0] == "statuses" && r.Method == http.MethodPost:
		var body Status
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			s.errorJSON(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		body.State, _ = raw["state"].(string)
		body.Context, _ = raw["context"].(string)
		body.Description, _ = raw["description"].(string)
		body.TargetURL, _ = raw["target_url"].(string)
		if !slices.Contains([]string{"pending", "success", "failure", "error"}, body.State) {
			s.errorJSON(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		if len([]rune(body.Description)) > 140 {
			s.errorJSON(w, http.StatusUnprocessableEntity, "description is too long (maximum is 140 characters)")
			return
		}
		if _, ok := s.resolve(repo, rest[1]); !ok {
			s.errorJSON(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+rest[1])
			return
		}
		k := strings.ToLower(owner+"/"+name) + "@" + rest[1]
		if s.statusLimit > 0 {
			n := 0
			for _, st := range s.statuses[k] {
				if st.Context == body.Context {
					n++
				}
			}
			if n >= s.statusLimit {
				s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"message":           "Validation Failed",
					"errors":            []map[string]any{{"resource": "Status", "code": "custom", "message": "This SHA and context has reached the maximum number of statuses."}},
					"documentation_url": "https://docs.github.com/rest/commits/statuses#create-a-commit-status",
				})
				return
			}
		}
		s.statuses[k] = append(s.statuses[k], body)
		s.writeJSON(w, http.StatusCreated, map[string]any{"id": len(s.statuses[k]), "state": body.State, "context": body.Context, "description": body.Description, "target_url": body.TargetURL})
	case len(rest) == 1 && rest[0] == "check-runs" && r.Method == http.MethodPost:
		if auth.kind != "installation" {
			s.errorJSON(w, http.StatusForbidden, "You must authenticate via a GitHub App.")
			return
		}
		var c CheckRun
		if !s.decodeCheckRun(w, r, &c) {
			return
		}
		if _, ok := s.resolve(repo, c.HeadSHA); !ok {
			s.errorJSON(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+c.HeadSHA)
			return
		}
		s.checkSeq++
		c.ID = s.checkSeq
		c.AppID = s.appID
		s.checkRuns = append(s.checkRuns, &c)
		s.writeJSON(w, http.StatusCreated, checkRunJSON(&c))
	case len(rest) == 2 && rest[0] == "check-runs" && r.Method == http.MethodPatch:
		if auth.kind != "installation" {
			s.errorJSON(w, http.StatusForbidden, "You must authenticate via a GitHub App.")
			return
		}
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		for _, c := range s.checkRuns {
			if c.ID == id {
				sha := c.HeadSHA
				if !s.decodeCheckRun(w, r, c) {
					return
				}
				c.HeadSHA = sha
				s.writeJSON(w, http.StatusOK, checkRunJSON(c))
				return
			}
		}
		s.errorJSON(w, http.StatusNotFound, "Not Found")
	default:
		s.errorJSON(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) decodeCheckRun(w http.ResponseWriter, r *http.Request, c *CheckRun) bool {
	var raw struct {
		Name        string  `json:"name"`
		HeadSHA     string  `json:"head_sha"`
		ExternalID  *string `json:"external_id"`
		Status      *string `json:"status"`
		Conclusion  *string `json:"conclusion"`
		DetailsURL  *string `json:"details_url"`
		CompletedAt *string `json:"completed_at"`
		Output      *struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		s.errorJSON(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	if raw.Conclusion != nil && raw.CompletedAt == nil {
		s.errorJSON(w, http.StatusUnprocessableEntity, "completed_at is required with conclusion")
		return false
	}
	if raw.Output != nil && (raw.Output.Title == "" || raw.Output.Summary == "") {
		s.errorJSON(w, http.StatusUnprocessableEntity, "output requires title and summary")
		return false
	}
	c.Name = raw.Name
	if raw.HeadSHA != "" {
		c.HeadSHA = raw.HeadSHA
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.ExternalID, raw.ExternalID)
	set(&c.Status, raw.Status)
	set(&c.Conclusion, raw.Conclusion)
	set(&c.DetailsURL, raw.DetailsURL)
	set(&c.CompletedAt, raw.CompletedAt)
	if raw.Output != nil {
		c.Title, c.Summary = raw.Output.Title, raw.Output.Summary
	}
	return true
}

func checkRunJSON(c *CheckRun) map[string]any {
	return map[string]any{
		"id": c.ID, "head_sha": c.HeadSHA, "name": c.Name, "external_id": c.ExternalID, "status": c.Status,
		"conclusion": nilIfEmpty(c.Conclusion), "details_url": c.DetailsURL, "app": map[string]any{"id": c.AppID},
		"output": map[string]any{"title": nilIfEmpty(c.Title), "summary": nilIfEmpty(c.Summary)},
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) pullJSON(r *http.Request, repo *Repo, pr Pull) map[string]any {
	labels := []map[string]any{}
	for i, l := range pr.Labels {
		labels = append(labels, map[string]any{"id": i + 1, "name": l, "color": "ededed", "default": false})
	}
	st := pr.State
	if st == "" {
		st = "open"
	}
	full := repo.Owner + "/" + repo.Name
	return map[string]any{
		"id":         100000 + pr.Number,
		"number":     pr.Number,
		"state":      st,
		"title":      pr.Title,
		"html_url":   fmt.Sprintf("https://github.example/%s/pull/%d", full, pr.Number),
		"url":        fmt.Sprintf("%s/repos/%s/pulls/%d", s.baseURL(r), full, pr.Number),
		"user":       map[string]any{"login": pr.User, "id": 7, "type": "User"},
		"labels":     labels,
		"draft":      pr.Draft,
		"merged_at":  nilIfEmpty(pr.MergedAt),
		"created_at": "2024-02-03T04:05:06Z",
		"head":       map[string]any{"label": repo.Owner + ":" + pr.Head, "ref": pr.Head, "sha": pr.HeadSHA, "repo": s.repoJSON(r, repo)},
		"base":       map[string]any{"label": repo.Owner + ":" + pr.Base, "ref": pr.Base, "sha": "basesha", "repo": s.repoJSON(r, repo)},
	}
}

func (s *Server) installationJSON(i Installation) map[string]any {
	return map[string]any{
		"id":                   i.ID,
		"app_id":               s.appID,
		"app_slug":             s.appSlug,
		"target_type":          i.AccountType,
		"account":              map[string]any{"login": i.Account, "id": i.ID * 10, "type": i.AccountType},
		"repository_selection": "all",
		"permissions":          map[string]any{"contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "write", "checks": "write"},
		"events":               []string{"push", "pull_request"},
	}
}

func (s *Server) serveFindInstallation(w http.ResponseWriter, r *http.Request, auth authInfo, kind, owner string) {
	if auth.kind != "jwt" {
		s.errorJSON(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	for _, i := range s.installations {
		if !strings.EqualFold(i.Account, owner) {
			continue
		}
		if (kind == "orgs" && i.AccountType != "Organization") || (kind == "users" && i.AccountType != "User") {
			continue
		}
		s.writeJSON(w, http.StatusOK, s.installationJSON(i))
		return
	}
	s.errorJSON(w, http.StatusNotFound, "Not Found")
}

func (s *Server) serveApp(w http.ResponseWriter, r *http.Request, auth authInfo, segs []string) {
	if auth.kind != "jwt" {
		s.errorJSON(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	switch {
	case len(segs) == 1 && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]any{"id": s.appID, "slug": s.appSlug, "name": s.appSlug})
	case len(segs) == 2 && segs[1] == "installations" && r.Method == http.MethodGet:
		var out []map[string]any
		for _, i := range s.installations {
			out = append(out, s.installationJSON(i))
		}
		if out == nil {
			out = []map[string]any{}
		}
		s.writeJSON(w, http.StatusOK, paginate(s, w, r, out))
	case len(segs) == 4 && segs[1] == "installations" && segs[3] == "access_tokens" && r.Method == http.MethodPost:
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		for _, i := range s.installations {
			if i.ID == id {
				s.tokenSeq++
				tok := fmt.Sprintf("ghs_fake%032d", s.tokenSeq)
				exp := s.now().Add(s.tokenTTL)
				s.tokens[tok] = issuedToken{installationID: id, expires: exp}
				s.writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339), "permissions": map[string]any{"contents": "read"}, "repository_selection": "all"})
				return
			}
		}
		s.errorJSON(w, http.StatusNotFound, "Not Found")
	default:
		s.errorJSON(w, http.StatusNotFound, "Not Found")
	}
}
