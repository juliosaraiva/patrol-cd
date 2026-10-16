package github

import (
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github/internal/fakegithub"
)

// TestAppFetchBudgetCoversRetryWaits: shared token minting and installation discovery
// run detached from callers; their deadline must leave room for at least one advised
// wait the retry policy accepts plus an attempt (a fixed 30s bound used to be shorter
// than the default 60s MaxWait).
func TestAppFetchBudgetCoversRetryWaits(t *testing.T) {
	t.Parallel()
	_, key, err := fakegithub.GenerateKey(2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		timeout time.Duration
		retry   scm.RetryPolicy
	}{
		{"defaults", 0, scm.RetryPolicy{}},
		{"long MaxWait", 10 * time.Second, scm.RetryPolicy{MaxWait: 3 * time.Minute}},
		{"retries disabled", 5 * time.Second, scm.RetryPolicy{MaxAttempts: 1}},
	}
	for _, tc := range tests {
		cfg := scm.Config{Timeout: tc.timeout, Retry: tc.retry,
			GitHubApp: &scm.GitHubAppConfig{AppID: 1, InstallationID: 2, PrivateKey: key}}
		p, err := NewProvider(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		d := cfg.WithDefaults()
		want := d.Timeout
		if d.Retry.MaxAttempts > 1 {
			want += d.Retry.MaxWait
		}
		if p.app.callBudget < want {
			t.Errorf("%s: fetch budget %v < MaxWait+Timeout %v", tc.name, p.app.callBudget, want)
		}
		if p.app.callBudget != d.Retry.CallBudget(d.Timeout) {
			t.Errorf("%s: fetch budget %v, want CallBudget %v", tc.name, p.app.callBudget, d.Retry.CallBudget(d.Timeout))
		}
	}
}
