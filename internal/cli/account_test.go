package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestAccountCommands(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")

	e.Run("account", "list").Succeeded(t).Contains(t, "NAME", "ENABLED", "CAPABILITIES", "admin", "alice", "login, apiKey", "ci")
	names := e.Run("account", "list", "-o", "name").Succeeded(t).Stdout
	for _, n := range []string{"admin", "alice", "bob", "ci", "nobody"} {
		if !strings.Contains(names, n+"\n") {
			t.Fatalf("account %s missing from %q", n, names)
		}
	}
	var accts []map[string]any
	if err := json.Unmarshal([]byte(e.Run("account", "list", "-o", "json").Succeeded(t).Stdout), &accts); err != nil || len(accts) < 5 {
		t.Fatalf("json list: %v %v", err, accts)
	}
	e.Run("account", "list", "-o", "toml").Failed(t, 20, "toml")

	e.Run("account", "get", "--account", "ci").Succeeded(t).Contains(t, "Name:               ci", "Capabilities:       apiKey", "Tokens:", "NONE")
	e.Run("account", "get").Succeeded(t).Contains(t, "Name:               admin")
	e.Run("account", "get", "--account", "nope").Failed(t, 20, "nope")

	// generate-token emits the token (its purpose) and the token authenticates as ci.
	tok := strings.TrimSpace(e.Run("account", "generate-token", "--account", "ci", "--id", "build-1", "--expires-in", "1h").Succeeded(t).Stdout)
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("not a JWT: %q", tok)
	}
	// The server learns about new tokens through its settings watch.
	e.WaitFor("ci token accepted", func() bool {
		return strings.Contains(e.RunAs(tok, "account", "get-user-info").Stdout, "Username: ci")
	})
	e.Run("account", "get", "--account", "ci").Succeeded(t).Contains(t, "build-1", "ISSUED AT", "EXPIRING AT")
	e.Run("account", "generate-token", "--account", "ci", "--expires-in", "soon").Failed(t, 20, "Invalid since format 'soon'")
	// Accounts without the apiKey capability cannot get tokens.
	e.Run("account", "generate-token", "--account", "nobody").Failed(t, 20)

	// delete-token revokes the token.
	e.Run("account", "delete-token", "--account", "ci", "build-1").Succeeded(t)
	e.WaitFor("ci token revoked", func() bool {
		return strings.Contains(e.RunAs(tok, "account", "get-user-info").Stdout, "Logged In: false")
	})
	e.Run("account", "get", "--account", "ci").Succeeded(t).Contains(t, "NONE")
	e.Run("account", "delete-token").Failed(t, 1)

	// get-user-info output formats.
	var ui map[string]any
	if err := json.Unmarshal([]byte(e.Run("account", "get-user-info", "-o", "json").Succeeded(t).Stdout), &ui); err != nil || ui["username"] != "admin" {
		t.Fatalf("user info json: %v %v", err, ui)
	}
	e.Run("account", "whoami", "-o", "yaml").Succeeded(t).Contains(t, "username: admin")
	e.Run("account", "get-user-info", "-o", "xml").Failed(t, 1, "Unknown output format: xml")

	// can-i.
	e.Run("account", "can-i", "create", "projects", "*").Succeeded(t).Contains(t, "yes")
	e.Run("account", "can-i", "create", "projects").Failed(t, 1)

	// session-token emits the stored token.
	cfgTok := e.ReadConfig().GetToken(e.Addr)
	if got := strings.TrimSpace(e.Run("account", "session-token").Succeeded(t).Stdout); got != cfgTok {
		t.Fatal("session-token did not print the stored token")
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(e.Run("account", "session-token", "-o", "json").Succeeded(t).Stdout), &st); err != nil {
		t.Fatal(err)
	}
	if st["type"] != "local" || st["username"] != "admin" || st["has_refresh_token"] != false || st["expires_at"] == nil {
		t.Fatalf("session-token json: %v", st)
	}
	e.Run("account", "session-token", "-o", "yaml").Failed(t, 20, "yaml")
}

func TestAccountAuthorization(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	alice := e.LoginToken("alice")
	nobody := e.LoginToken("nobody")

	// alice is read-only: listing works, tokens for others do not.
	e.RunAs(alice, "account", "list").Succeeded(t).Contains(t, "alice")
	e.RunAs(alice, "account", "generate-token", "--account", "ci").Failed(t, 20, "permission denied")
	e.RunAs(alice, "account", "can-i", "get", "projects", "*").Succeeded(t).Contains(t, "yes")
	e.RunAs(alice, "account", "can-i", "delete", "projects", "*").Succeeded(t).Contains(t, "no")
	// Every account may manage its own tokens.
	own := strings.TrimSpace(e.RunAs(alice, "account", "generate-token", "--id", "mine").Succeeded(t).Stdout)
	e.WaitFor("alice token accepted", func() bool {
		return strings.Contains(e.RunAs(own, "account", "get-user-info").Stdout, "Username: alice")
	})
	e.RunAs(alice, "account", "delete-token", "mine").Succeeded(t)
	// nobody has no grants: only its own account is listed.
	e.RunAs(nobody, "account", "list").Succeeded(t).Contains(t, "nobody").NotContains(t, "alice")
	e.RunAs(nobody, "account", "get", "--account", "alice").Failed(t, 20, "permission denied")
	e.RunAs(nobody, "account", "can-i", "get", "projects", "*").Succeeded(t).Contains(t, "no")
	// Anonymous requests are rejected.
	e.RunWith(clitest.RunOptions{}, append(e.ServerFlags(), "account", "list")...).Failed(t, 20)
}

func TestAccountUpdatePassword(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.Login("bob")
	const newPass = "Bob-New-Pass-2!"
	e.Run("account", "update-password", "--current-password", "wrong", "--new-password", newPass).Failed(t, 20)
	r := e.Run("account", "update-password", "--current-password", clitest.Password, "--new-password", newPass).Succeeded(t)
	r.Contains(t, "Password updated", "Context '"+e.Addr+"' updated").NotContains(t, newPass, clitest.Password)
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: bob")
	e.Run("login", e.Addr, "--plaintext", "--username", "bob", "--password", newPass).Succeeded(t)

	// Interactive: current password, new password and confirmation from the prompts.
	const third = "Bob-Third-Pass-3!"
	r = e.RunWith(clitest.RunOptions{Stdin: newPass + "\n" + third + "\n" + third + "\n"}, "account", "update-password").Succeeded(t)
	if !strings.Contains(r.Stderr, "*** Enter new password for user bob") || !strings.Contains(r.Stderr, "*** Confirm new password for user bob") {
		t.Fatalf("prompts: %q", r.Stderr)
	}
	// Mismatched confirmation.
	e.RunWith(clitest.RunOptions{Stdin: third + "\nA-Pass-4444!\nB-Pass-5555!\n"}, "account", "update-password").Failed(t, 20, "Passwords do not match")
}

func TestAccountBcrypt(t *testing.T) {
	t.Parallel()
	r := clitest.NewRunner(t)
	h := r.Run("account", "bcrypt", "--password", "s3cret").Succeeded(t).Stdout
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("s3cret")) != nil {
		t.Fatalf("hash does not verify: %q", h)
	}
	h = r.RunWith(clitest.RunOptions{Stdin: "from-stdin\n"}, "account", "bcrypt").Succeeded(t).Stdout
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("from-stdin")) != nil {
		t.Fatalf("stdin hash does not verify: %q", h)
	}
	// Blank answers are asked again (upstream); without more input the command fails.
	r.RunWith(clitest.RunOptions{Stdin: "\n"}, "account", "bcrypt").Failed(t, 20, "no input available to answer the prompt")
	r.Run("account", "bcrypt", "extra").Failed(t, 1)
}

func TestAccountSessionTokenWithoutLogin(t *testing.T) {
	t.Parallel()
	clitest.NewRunner(t).Run("account", "session-token").Failed(t, 1, "No configuration found. Please login first with 'patrol login'")
}
