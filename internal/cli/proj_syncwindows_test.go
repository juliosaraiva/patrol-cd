package cli_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestProjectSyncWindows(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "w").Succeeded(t)

	e.Run("proj", "windows", "add", "w", "-k", "deny", "--schedule", "0 22 * * *", "--duration", "1h", "--applications", "prod-*,web",
		"--namespaces", "default", "--clusters", "in-cluster", "--time-zone", "Europe/Lisbon", "--description", "nightly").Succeeded(t)
	// A window that is always active (every minute, one hour long).
	e.Run("proj", "windows", "add", "w", "--kind", "allow", "--schedule", "* * * * *", "--duration", "1h", "--namespaces", "*", "--manual-sync", "--use-and-operator").Succeeded(t)
	ws := e.GetProject("w").Spec.SyncWindows
	if len(ws) != 2 || ws[0].Kind != "deny" || ws[0].TimeZone != "Europe/Lisbon" || len(ws[0].Applications) != 2 || ws[0].Description != "nightly" ||
		!ws[1].ManualSync || !ws[1].UseAndOperator {
		t.Fatalf("windows: %+v", ws)
	}
	// Validation.
	e.Run("proj", "windows", "add", "w", "-k", "deny", "--schedule", "0 22 * * *", "--duration", "1h").Failed(t, 20,
		"cannot create window: require kind, schedule, duration and one or more of applications, namespaces and clusters")
	e.Run("proj", "windows", "add", "w", "-k", "maybe", "--schedule", "0 22 * * *", "--duration", "1h", "--namespaces", "x").Failed(t, 20, "maybe")
	e.Run("proj", "windows", "add", "w", "-k", "deny", "--schedule", "not cron", "--duration", "1h", "--namespaces", "x").Failed(t, 20)

	// list with the computed status.
	l := e.Run("proj", "windows", "list", "w").Succeeded(t).Stdout
	for _, w := range []string{"ID", "STATUS", "KIND", "SCHEDULE", "DURATION", "APPLICATIONS", "NAMESPACES", "CLUSTERS", "MANUALSYNC", "SYNCOVERRUN", "TIMEZONE", "USEANDOPERATOR", "prod-*,web", "Europe/Lisbon"} {
		if !strings.Contains(l, w) {
			t.Fatalf("list lacks %q:\n%s", w, l)
		}
	}
	if !regexp.MustCompile(`(?m)^1\s+Active\s+allow\s`).MatchString(l) {
		t.Fatalf("always-on window not active:\n%s", l)
	}
	e.Run("proj", "windows", "list", "w", "-o", "yaml").Succeeded(t).Contains(t, "schedule: 0 22 * * *")
	e.Run("proj", "windows", "list", "w", "-o", "json").Succeeded(t).Contains(t, `"kind": "deny"`)

	// toggles.
	e.Run("proj", "windows", "enable-manual-sync", "w", "0").Succeeded(t)
	e.Run("proj", "windows", "enable-sync-overrun", "w", "0").Succeeded(t)
	if w := e.GetProject("w").Spec.SyncWindows[0]; !w.ManualSync || !w.SyncOverrun {
		t.Fatalf("toggles: %+v", w)
	}
	e.Run("proj", "windows", "disable-manual-sync", "w", "0").Succeeded(t)
	e.Run("proj", "windows", "disable-sync-overrun", "w", "0").Succeeded(t)
	if w := e.GetProject("w").Spec.SyncWindows[0]; w.ManualSync || w.SyncOverrun {
		t.Fatalf("toggles: %+v", w)
	}
	e.Run("proj", "windows", "enable-manual-sync", "w", "7").Failed(t, 20, "window with id '7' not found")
	e.Run("proj", "windows", "enable-manual-sync", "w", "x").Failed(t, 20, `invalid window id "x"`)

	// update.
	e.Run("proj", "windows", "update", "w", "0", "--schedule", "0 23 * * *", "--duration", "2h", "--description", "later").Succeeded(t)
	if w := e.GetProject("w").Spec.SyncWindows[0]; w.Schedule != "0 23 * * *" || w.Duration != "2h" || w.Description != "later" {
		t.Fatalf("update: %+v", w)
	}
	e.Run("proj", "windows", "update", "w", "0").Failed(t, 20, "cannot update: require one or more of schedule, duration, application, namespace, cluster or description")

	// delete (optional prompt).
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "--prompts-enabled", "proj", "windows", "delete", "w", "0").Succeeded(t).Contains(t, "The command to delete the sync window was cancelled")
	e.Run("proj", "windows", "delete", "w", "0").Succeeded(t)
	if ws := e.GetProject("w").Spec.SyncWindows; len(ws) != 1 || ws[0].Kind != "allow" {
		t.Fatalf("delete: %+v", ws)
	}
	e.Run("proj", "windows", "delete", "w", "5").Failed(t, 20, "window with id '5' not found")

	// Authorization.
	alice := e.LoginToken("alice")
	e.RunAs(alice, "proj", "windows", "list", "w").Succeeded(t)
	e.RunAs(alice, "proj", "windows", "delete", "w", "0").Failed(t, 20, "permission denied")
}

func TestProjectSourceIntegrity(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "si").Succeeded(t)
	base := []string{"proj", "source-integrity", "git", "policies"}
	run := func(args ...string) clitest.Result { return e.Run(append(append([]string{}, base...), args...)...) }

	run("list", "si").Failed(t, 20, `no source integrity git policies defined for project "si"`)
	// A key that is not in the keyring is accepted with a warning.
	r := run("add", "si", "--repo-url", "https://git.example.com/*", "--gpg-mode", "strict", "--gpg-key", "4AEE18F83AFDEB23").Succeeded(t)
	if !strings.Contains(r.Stdout+r.Stderr, "Warning: Following GPG keys are not in repo-server keyring: 4AEE18F83AFDEB23") {
		t.Fatalf("no keyring warning: %+v", r)
	}
	run("add", "si", "--repo-url", "https://other.example.com/*", "--gpg-mode", "head", "--gpg-key", "D56C4FCA57A46444").Succeeded(t)
	run("add", "si", "--repo-url", "x", "--gpg-mode", "loose").Failed(t, 20, "gpg-mode must be one of: strict, head, none")
	run("add", "si", "--repo-url", "x").Failed(t, 20, "gpg-mode must be set")
	run("add", "si", "--repo-url", "x", "--gpg-mode", "strict", "--gpg-key", "bad").Failed(t, 20, "invalid GPG key ID 'bad'")

	l := run("list", "si").Succeeded(t).Stdout
	if !regexp.MustCompile(`(?m)^0\s+strict\s+4AEE18F83AFDEB23\s+https://git.example.com/\*$`).MatchString(l) ||
		!regexp.MustCompile(`(?m)^1\s+head\s+D56C4FCA57A46444\s+https://other.example.com/\*$`).MatchString(l) {
		t.Fatalf("list:\n%s", l)
	}

	// update: mode, keys and URLs (explicit confirmation skipped with -y).
	run("update", "si", "1", "--gpg-mode", "none", "--add-repo-url", "https://third.example.com/*", "--delete-gpg-key", "D56C4FCA57A46444", "-y").Succeeded(t)
	pol := e.GetProject("si").Spec.SourceIntegrity.Git.Policies[1]
	if pol.GPG == nil || pol.GPG.Mode != "none" || len(pol.Repos) != 2 || len(pol.GPG.Keys) != 0 {
		t.Fatalf("update: %+v", pol)
	}
	run("update", "si", "1", "--gpg-key", "4AEE18F83AFDEB23", "--add-gpg-key", "D56C4FCA57A46444", "-y").Failed(t, 20, "option --gpg-key, cannot be combined with --add-gpg-key or --delete-gpg-key")
	run("update", "si", "9", "--gpg-mode", "strict", "-y").Failed(t, 20, "the POLICY_ID 9 is out of range (0-1)")
	// Without -y the confirmation is mandatory; declining aborts.
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, append(base, "update", "si", "0", "--gpg-mode", "head")...).Succeeded(t).Contains(t, "Aborted by user.")
	if m := e.GetProject("si").Spec.SourceIntegrity.Git.Policies[0].GPG.Mode; m != "strict" {
		t.Fatalf("declined update applied: %s", m)
	}
	// No answer (empty input, e.g. CI without -y) fails instead of reporting success.
	e.RunWith(clitest.RunOptions{}, append(base, "update", "si", "0", "--gpg-mode", "head")...).
		Failed(t, 20, "confirm update: no input available to answer the prompt").NotContains(t, "Aborted by user.")
	if m := e.GetProject("si").Spec.SourceIntegrity.Git.Policies[0].GPG.Mode; m != "strict" {
		t.Fatalf("unanswered update applied: %s", m)
	}

	// delete.
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, append(base, "delete", "si", "0")...).Succeeded(t).Contains(t, "Aborted by user.")
	e.RunWith(clitest.RunOptions{}, append(base, "delete", "si", "0")...).
		Failed(t, 20, "confirm deletion: no input available to answer the prompt").NotContains(t, "Aborted by user.")
	if ps := e.GetProject("si").Spec.SourceIntegrity.Git.Policies; len(ps) != 2 {
		t.Fatalf("unanswered delete applied: %+v", ps)
	}
	run("delete", "si", "0", "-y").Succeeded(t)
	if ps := e.GetProject("si").Spec.SourceIntegrity.Git.Policies; len(ps) != 1 || ps[0].Repos[0].URL != "https://other.example.com/*" {
		t.Fatalf("delete: %+v", ps)
	}
	run("delete", "si", "x", "-y").Failed(t, 20, "invalid POLICY_ID 'x'")

	alice := e.LoginToken("alice")
	e.RunAs(alice, append(base, "list", "si")...).Succeeded(t)
	e.RunAs(alice, append(base, "delete", "si", "0", "-y")...).Failed(t, 20, "permission denied")
}
