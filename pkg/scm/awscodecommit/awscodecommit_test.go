package awscodecommit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/awscodecommit"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
)

const staticKey = "AKIASTATICTEST00001"

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type seed struct {
	f        *fakeAWS
	svcNames []string
	branches map[string]string
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeAWS(t)
	s := &seed{f: f}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		f.add(&fakeRepo{Name: name, ID: fmt.Sprintf("id-%d", i), DefaultBranch: "main", Tags: map[string]string{"team": "a"},
			Branches: []fakeBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	f.add(&fakeRepo{Name: "platform", ID: "id-platform", DefaultBranch: "main", Tags: map[string]string{"team": "platform", "env": "prod"},
		Branches: []fakeBranch{{Name: "main", SHA: sha(1)}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)}, {Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)}},
		Files: map[string][]string{
			sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "vendor/lib@", "current->"},
			sha(2): {"apps/b/values.yaml"},
		}})
	f.add(&fakeRepo{Name: "empty", ID: "id-empty"})
	return s
}

var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

// awsConfig is an aws.Config with static credentials and the fake as BaseEndpoint.
func (s *seed) awsConfig() *aws.Config {
	return &aws.Config{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(staticKey, "static-secret", ""),
		BaseEndpoint: aws.String(s.f.srv.URL),
	}
}

func scmConfig() scm.Config {
	return scm.Config{Type: awscodecommit.Type, Name: "test", RateLimit: scm.RateLimit{RPS: -1}, Retry: testRetry, Timeout: 10 * time.Second}
}

func newProvider(t testing.TB, s *seed, opts awscodecommit.Options) *awscodecommit.Provider {
	t.Helper()
	if opts.AWSConfig == nil {
		opts.AWSConfig = s.awsConfig()
	}
	p, err := awscodecommit.NewProvider(context.Background(), scmConfig(), opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

type ccFixture struct {
	s    *seed
	data conformance.Data
}

func (f *ccFixture) Data() conformance.Data { return f.data }

func (f *ccFixture) WebhookDelivery(t testing.TB, typ scm.EventType, _ string, _ []byte) (http.Header, []byte) {
	t.Fatalf("CodeCommit has no webhooks (asked for %q)", typ)
	return nil, nil
}

func (f *ccFixture) SignatureHeaders() []string { return nil }

// InjectRateLimit makes every API call fail with ThrottlingException (AWS sends no reset
// hint; the provider reports a fixed RetryAfter).
func (f *ccFixture) InjectRateLimit(time.Duration) { f.s.f.setThrottle(true) }

func (f *ccFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *ccFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		return newProvider(t, s, awscodecommit.Options{}), &ccFixture{s: s, data: conformance.Data{
			Organization: account,
			Repositories: append(append([]string{}, s.svcNames...), "platform"),
			// Uninitialized repositories (no default branch) are skipped before filters.
			Repo:          scm.RepoRef{Owner: account, Name: "platform"},
			DefaultBranch: "main",
			Branches:      s.branches,
			ExistingPath:  "apps/a",
			MissingPath:   "apps/zzz",
			MissingRepo:   scm.RepoRef{Owner: account, Name: "nothing"},
		}}
	}, conformance.Options{Skip: []string{
		// CodeCommit has no organization scope (upstream lists the repositories of the
		// credentials' account and region): RepoQuery.Organization is ignored, so an
		// unknown organization cannot be reported as NotFound.
		"Repositories/NotFound",
	}})
}

func TestListRepositoriesFieldsAndProtocols(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s, awscodecommit.Options{})
	only := func(r scm.Repository) bool { return r.Name == "platform" }
	for proto, want := range map[scm.CloneProtocol]string{
		scm.CloneProtocolDefault:             "ssh://git-codecommit.eu-west-1.amazonaws.com/v1/repos/platform",
		scm.CloneProtocolHTTPS:               "https://git-codecommit.eu-west-1.amazonaws.com/v1/repos/platform",
		awscodecommit.CloneProtocolHTTPSFIPS: "https://git-codecommit-fips.eu-west-1.amazonaws.com/v1/repos/platform",
	} {
		got, err := p.ListRepositories(t.Context(), scm.RepoQuery{CloneProtocol: proto, Match: only})
		if err != nil || len(got) != 1 || got[0].URL != want {
			t.Fatalf("protocol %q: %+v, %v", proto, got, err)
		}
		r := got[0]
		if r.Organization != account || r.RepositoryID != "id-platform" || r.SHA != sha(1) || r.Branch != "main" || r.Labels == nil || len(r.Labels) != 0 {
			t.Fatalf("platform = %+v", r)
		}
	}
	_, err := p.ListRepositories(t.Context(), scm.RepoQuery{CloneProtocol: "git"})
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "unknown clone protocol for codecommit git") {
		t.Fatalf("unknown protocol: %v", err)
	}
	keys, targets, _, _ := s.f.snapshot()
	for i, k := range keys {
		if k != staticKey {
			t.Fatalf("request %d (%s) signed with %q", i, targets[i], k)
		}
	}
}

func TestTagFilters(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s, awscodecommit.Options{TagFilters: []awscodecommit.TagFilter{
		{Key: "team", Value: "platform"}, {Key: "team", Value: "a"}, {Key: "env"}, {Key: "team", Value: "a"},
	}})
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{})
	if err != nil || len(got) != 1 || got[0].Name != "platform" {
		t.Fatalf("tag filtered: %+v, %v", got, err)
	}
	_, targets, queries, _ := s.f.snapshot()
	if slices.Contains(targets, "CodeCommit_20150413.ListRepositories") {
		t.Fatal("tag filters must use the tagging API instead of ListRepositories")
	}
	var q struct {
		ResourceTypeFilters []string
		TagFilters          []struct {
			Key    string
			Values []string
		}
	}
	if err := json.Unmarshal(queries[0], &q); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(q.ResourceTypeFilters, []string{"codecommit:repository"}) || len(q.TagFilters) != 2 ||
		q.TagFilters[0].Key != "env" || len(q.TagFilters[0].Values) != 0 ||
		q.TagFilters[1].Key != "team" || !slices.Equal(q.TagFilters[1].Values, []string{"platform", "a"}) {
		t.Fatalf("tagging request = %s", queries[0])
	}
	if _, err := awscodecommit.NewProvider(t.Context(), scmConfig(), awscodecommit.Options{AWSConfig: s.awsConfig(), TagFilters: []awscodecommit.TagFilter{{Value: "x"}}}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("empty tag key: %v", err)
	}
}

func TestAssumeRole(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	const role = "arn:aws:iam::210987654321:role/patrol-discovery"
	p := newProvider(t, s, awscodecommit.Options{Role: role})
	if _, err := p.ListBranches(t.Context(), scm.RepoRef{Name: "platform"}, true); err != nil {
		t.Fatal(err)
	}
	keys, targets, _, assumed := s.f.snapshot()
	if !slices.Equal(assumed, []string{role}) {
		t.Fatalf("assumed roles = %v", assumed)
	}
	for i, tgt := range targets {
		if tgt == "" {
			if keys[i] != staticKey {
				t.Fatalf("AssumeRole must be signed with the base credentials, got %q", keys[i])
			}
			continue
		}
		if keys[i] != "ASIAASSUMEDROLE0001" {
			t.Fatalf("%s signed with %q, want the assumed role's credentials", tgt, keys[i])
		}
	}
}

func TestPathExists(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s, awscodecommit.Options{})
	ref := scm.RepoRef{Owner: account, Name: "platform"}
	for _, tc := range []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a/values.yaml", true}, // file
		{"main", "apps/a", true},             // folder
		{"main", "apps/a/", true},            // trailing slash
		{"main", "vendor/lib", true},         // submodule
		{"main", "current", true},            // symbolic link
		{"main", "/", true},                  // root, no API call
		{"", "README.md", true},
		{"main", "apps/zzz", false},       // missing in an existing folder
		{"main", "nope/x", false},         // missing folder
		{"feature/a", "apps/b", true},     // other branch
		{"no-such-branch", "apps", false}, // missing revision
	} {
		if got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path); err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v; want %v", tc.rev, tc.path, got, err, tc.want)
		}
	}
	if ok, err := p.PathExists(t.Context(), scm.RepoRef{Name: "missing"}, "main", "a"); ok || err != nil {
		t.Errorf("missing repository: %v, %v", ok, err)
	}
}

func TestThrottlingMapsToRateLimited(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.setThrottle(true)
	_, err := newProvider(t, s, awscodecommit.Options{}).ListBranches(t.Context(), scm.RepoRef{Name: "platform"}, true)
	if !scm.IsRateLimited(err) {
		t.Fatalf("err = %v", err)
	}
	if ra, ok := scm.RetryAfter(err); !ok || ra <= 0 {
		t.Fatalf("RetryAfter = %v, %v", ra, ok)
	}
	_, targets, _, _ := s.f.snapshot()
	if len(targets) != testRetry.MaxAttempts {
		t.Fatalf("SDK retries must be bounded by Retry.MaxAttempts: %d attempts", len(targets))
	}
}

func TestConstructionErrors(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := scmConfig()
	cfg.APIURL = "https://codecommit.example.com"
	if _, err := awscodecommit.NewProvider(t.Context(), cfg, awscodecommit.Options{AWSConfig: s.awsConfig()}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("API URL: %v", err)
	}
	noRegion := s.awsConfig()
	noRegion.Region = ""
	if _, err := awscodecommit.NewProvider(t.Context(), scmConfig(), awscodecommit.Options{AWSConfig: noRegion}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("missing region: %v", err)
	}
	if p, err := awscodecommit.NewProvider(t.Context(), scmConfig(), awscodecommit.Options{AWSConfig: noRegion, Region: "us-east-1"}); err != nil || p == nil {
		t.Fatalf("region option: %v", err)
	}
}

// TestDefaultConfigFromEnvironment covers the registry path, which loads the default
// AWS configuration (here from hermetic environment variables pointing at the fake).
func TestDefaultConfigFromEnvironment(t *testing.T) {
	s := newSeed(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENVIRONMENT0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", region)
	t.Setenv("AWS_ENDPOINT_URL", s.f.srv.URL)
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	r := scm.NewRegistry()
	if err := awscodecommit.Register(r); err != nil {
		t.Fatal(err)
	}
	p, err := r.New(t.Context(), scmConfig())
	if err != nil {
		t.Fatal(err)
	}
	bl, err := scm.As[scm.BranchLister](p)
	if err != nil {
		t.Fatal(err)
	}
	if bs, err := bl.ListBranches(t.Context(), scm.RepoRef{Name: "platform"}, false); err != nil || len(bs) != 1 || bs[0].SHA != sha(1) {
		t.Fatalf("default branch: %v, %v", bs, err)
	}
	keys, _, _, _ := s.f.snapshot()
	if len(keys) == 0 || keys[0] != "AKIAENVIRONMENT0001" {
		t.Fatalf("environment credentials not used: %v", keys)
	}
}

func TestFIPSCloneURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://git-codecommit.us-east-1.amazonaws.com/v1/repos/r":      "https://git-codecommit-fips.us-east-1.amazonaws.com/v1/repos/r",
		"https://git-codecommit-fips.us-east-1.amazonaws.com/v1/repos/r": "https://git-codecommit-fips.us-east-1.amazonaws.com/v1/repos/r",
	} {
		if got, err := awscodecommit.FIPSCloneURL(in); err != nil || got != want {
			t.Errorf("FIPSCloneURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := awscodecommit.FIPSCloneURL("ssh://git-codecommit.us-east-1.amazonaws.com/v1/repos/r"); err == nil {
		t.Error("non-HTTPS URL accepted")
	}
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z]{2}-[a-z]{4,9}-[1-3]\.amazonaws\.com/v1/repos/[A-Za-z0-9._-]{1,20}`).Draw(t, "rest")
		once, err := awscodecommit.FIPSCloneURL("https://git-codecommit." + host)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := awscodecommit.FIPSCloneURL(once)
		if err != nil || twice != once || !strings.HasSuffix(once, host) {
			t.Fatalf("FIPS rewrite not idempotent: %q → %q", once, twice)
		}
	})
}
