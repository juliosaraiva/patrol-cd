package cli_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestProjectRoles(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "p", "-d", "https://kubernetes.default.svc,apps", "-s", "*").Succeeded(t)

	e.Run("proj", "role", "create", "p", "ci", "--description", "CI role").Succeeded(t).Contains(t, "Role 'ci' created")
	e.Run("proj", "role", "create", "p", "ci").Succeeded(t).Contains(t, "Role 'ci' already exists")
	e.Run("proj", "role", "create", "missing", "ci").Failed(t, 20, "not found")

	// Policies.
	e.Run("proj", "role", "add-policy", "p", "ci", "-a", "get", "-o", "*").Succeeded(t)
	e.Run("proj", "role", "add-policy", "p", "ci", "-a", "sync", "-o", "web", "-p", "deny").Succeeded(t)
	e.Run("proj", "role", "add-policy", "p", "ci", "-a", "get", "-o", "*", "-r", "logs").Succeeded(t)
	want := []string{
		"p, proj:p:ci, applications, get, p/*, allow",
		"p, proj:p:ci, applications, sync, p/web, deny",
		"p, proj:p:ci, logs, get, p/*, allow",
	}
	if got := e.GetProject("p").Spec.Roles[0].Policies; !slices.Equal(got, want) {
		t.Fatalf("policies:\n%v\nwant\n%v", got, want)
	}
	// Non project-scoped resources print the help and fail.
	e.Run("proj", "role", "add-policy", "p", "ci", "-a", "get", "-o", "*", "-r", "accounts").Failed(t, 1)
	e.Run("proj", "role", "add-policy", "p", "nope", "-a", "get", "-o", "*").Failed(t, 20, "role 'nope' does not exist in project 'p'")
	e.Run("proj", "role", "remove-policy", "p", "ci", "-a", "sync", "-o", "web", "-p", "deny").Succeeded(t)
	if got := e.GetProject("p").Spec.Roles[0].Policies; len(got) != 2 {
		t.Fatalf("policy not removed: %v", got)
	}

	// Groups.
	e.Run("proj", "role", "add-group", "p", "ci", "devs").Succeeded(t).Contains(t, "Group 'devs' added to role 'ci'")
	e.Run("proj", "role", "add-group", "p", "ci", "devs").Succeeded(t).Contains(t, "Group 'devs' already present in role 'ci'")
	e.Run("proj", "role", "add-group", "p", "ci", "ops").Succeeded(t)
	e.Run("proj", "role", "remove-group", "p", "ci", "ops").Succeeded(t).Contains(t, "Group 'ops' removed from role 'ci'")
	e.Run("proj", "role", "remove-group", "p", "ci", "ops").Succeeded(t).Contains(t, "Group 'ops' not present in role 'ci'")

	// list and get.
	e.Run("proj", "role", "list", "p").Succeeded(t).Contains(t, "ROLE-NAME", "DESCRIPTION", "ci", "CI role")
	e.Run("proj", "role", "list", "p", "-o", "name").Succeeded(t).Contains(t, "ci\n")
	e.Run("proj", "role", "list", "p", "-o", "json").Succeeded(t).Contains(t, `"name": "ci"`)
	g := e.Run("proj", "role", "get", "p", "ci").Succeeded(t)
	g.Contains(t, "Role Name:", "ci", "Description:", "CI role", "Policies:",
		"p, proj:p:ci, projects, get, p, allow", "p, proj:p:ci, applications, get, p/*, allow", "g, devs, proj:p:ci",
		"Groups:", "  - devs", "JWT Tokens:")

	// Tokens.
	r := e.Run("proj", "role", "create-token", "p", "ci", "-e", "1h", "-i", "tok-1").Succeeded(t)
	r.Contains(t, "Create token succeeded for proj:p:ci.", "  ID: tok-1", "  Issued At: ", "  Expires At: ", "  Token: ")
	tok := strings.TrimPrefix(regexp.MustCompile(`(?m)^  Token: (\S+)$`).FindString(r.Stdout), "  Token: ")
	if tok == "" {
		t.Fatalf("no token in %q", r.Stdout)
	}
	only := strings.TrimSpace(e.Run("proj", "role", "token-create", "p", "ci", "-t", "-i", "tok-2").Succeeded(t).Stdout)
	if strings.Count(only, ".") != 2 || strings.Contains(only, " ") {
		t.Fatalf("--token-only output: %q", only)
	}
	e.Run("proj", "role", "create-token", "p", "ci", "-e", "never").Failed(t, 20)

	// The project token carries the role's permissions (applied through the enforcer's
	// project watch).
	e.WaitFor("project token authorised", func() bool {
		return strings.TrimSpace(e.RunAs(tok, "account", "can-i", "get", "applications", "p/web").Stdout) == "yes"
	})
	e.RunAs(tok, "account", "can-i", "get", "applications", "other/web").Succeeded(t).Contains(t, "no")
	e.RunAs(tok, "account", "can-i", "delete", "applications", "p/web").Succeeded(t).Contains(t, "no")
	e.RunAs(tok, "proj", "get", "p").Succeeded(t).Contains(t, "Name:")

	lt := e.Run("proj", "role", "list-tokens", "p", "ci").Succeeded(t)
	lt.Contains(t, "ID", "ISSUED AT", "EXPIRES AT", "tok-1", "tok-2")
	ux := e.Run("proj", "role", "list-tokens", "p", "ci", "-u").Succeeded(t).Stdout
	m := regexp.MustCompile(`(?m)^tok-1\s+(\d+)\s`).FindStringSubmatch(ux)
	if m == nil {
		t.Fatalf("unix timestamps: %q", ux)
	}
	e.Run("proj", "role", "delete-token", "p", "ci", m[1]).Succeeded(t)
	e.Run("proj", "role", "delete-token", "p", "ci", "0", "--id", "tok-2").Succeeded(t)
	e.Run("proj", "role", "list-tokens", "p", "ci").Succeeded(t).Contains(t, "No tokens for p.ci")
	e.WaitFor("project token revoked", func() bool {
		return e.RunAs(tok, "proj", "get", "p").Code != 0
	})
	e.Run("proj", "role", "delete-token", "p", "ci", "notanumber").Failed(t, 20)

	// Authorization.
	alice := e.LoginToken("alice")
	e.RunAs(alice, "proj", "role", "list", "p").Succeeded(t).Contains(t, "ci")
	e.RunAs(alice, "proj", "role", "create", "p", "x").Failed(t, 20, "permission denied")
	e.RunAs(alice, "proj", "role", "create-token", "p", "ci").Failed(t, 20, "permission denied")

	// Deleting the role.
	e.Run("proj", "role", "delete", "p", "ci").Succeeded(t).Contains(t, "Role 'ci' deleted")
	e.Run("proj", "role", "delete", "p", "ci").Succeeded(t).Contains(t, "Role 'ci' does not exist in project")
	if len(e.GetProject("p").Spec.Roles) != 0 {
		t.Fatal("role not deleted")
	}
}
