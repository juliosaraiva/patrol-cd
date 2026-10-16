package main

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/admin"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
)

func TestRegisteredGroups(t *testing.T) {
	t.Parallel()
	r := clitest.NewRunner(t, registrars()...)
	r.Run("--help").Succeeded(t).Contains(t, "admin", "app", "appset", "proj", "login")
	r.Run("admin", "--help").Succeeded(t).Contains(t, "requires direct Kubernetes access")
	r.Run("app", "--help").Succeeded(t).Contains(t, "Manage applications")
}

func TestAdaptAdminErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err    error
		code   int
		silent bool
	}{
		{nil, 0, false},
		{errors.New("boom"), 1, false},
		{&admin.ExitError{Code: 2, Err: errors.New("incompatible")}, 2, false},
		{&admin.ExitError{Code: 1}, 1, true},
	} {
		c := &cobra.Command{Use: "admin", RunE: func(*cobra.Command, []string) error { return tc.err }}
		adaptAdminErrors(c)
		err := c.RunE(c, nil)
		code, printed := cmdutil.ExitCodeOf(err)
		if code != tc.code || (err != nil && printed == tc.silent) {
			t.Fatalf("%v: code %d printed %v", tc.err, code, printed)
		}
	}
}
