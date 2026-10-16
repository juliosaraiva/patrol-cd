package stack

import (
	"encoding/json"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// patchData merge-patches the data of a ConfigMap or Secret of the control-plane namespace
// (a nil value removes the key; Secret values are given in clear text and sent as
// stringData).
func (s *Stack) patchData(t testing.TB, kind, name string, data map[string]*string) {
	t.Helper()
	field := "data"
	if kind == "Secret" {
		field = "stringData"
		// stringData cannot remove keys: removals go through data.
		removals := map[string]any{}
		for k, v := range data {
			if v == nil {
				removals[k] = nil
			}
		}
		if len(removals) > 0 {
			b, err := json.Marshal(map[string]any{"data": removals})
			if err != nil {
				t.Fatal(err)
			}
			s.control.MergePatch(t, "v1", kind, s.Namespace, name, string(b))
		}
	}
	m := map[string]any{}
	for k, v := range data {
		if v == nil {
			if kind != "Secret" {
				m[k] = nil
			}
			continue
		}
		m[k] = *v
	}
	if len(m) == 0 {
		return
	}
	b, err := json.Marshal(map[string]any{field: m})
	if err != nil {
		t.Fatal(err)
	}
	s.control.MergePatch(t, "v1", kind, s.Namespace, name, string(b))
}

func values(m map[string]string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = Ptr(v)
	}
	return out
}

// UpdateSettings sets patrol-cm entries (the components reload them without a restart).
func (s *Stack) UpdateSettings(t testing.TB, entries map[string]string) {
	t.Helper()
	s.patchData(t, "ConfigMap", settings.DefaultConfigMapName, values(entries))
}

// RemoveSettings removes patrol-cm entries.
func (s *Stack) RemoveSettings(t testing.TB, keys ...string) {
	t.Helper()
	m := map[string]*string{}
	for _, k := range keys {
		m[k] = nil
	}
	s.patchData(t, "ConfigMap", settings.DefaultConfigMapName, m)
}

// UpdateSecret sets patrol-secret entries.
func (s *Stack) UpdateSecret(t testing.TB, entries map[string]string) {
	t.Helper()
	s.patchData(t, "Secret", settings.DefaultSecretName, values(entries))
}

// SetRBAC replaces patrol-rbac-cm policy.csv and policy.default.
func (s *Stack) SetRBAC(t testing.TB, policy, defaultRole string) {
	t.Helper()
	s.patchData(t, "ConfigMap", settings.DefaultRBACConfigMapName, map[string]*string{
		settings.RBACKeyPolicyCSV: Ptr(policy), settings.RBACKeyPolicyDefault: Ptr(defaultRole),
	})
}

// AppendRBAC appends lines to patrol-rbac-cm policy.csv.
func (s *Stack) AppendRBAC(t testing.TB, lines string) {
	t.Helper()
	s.opts.RBACPolicy += lines
	s.SetRBAC(t, s.opts.RBACPolicy, s.opts.RBACDefault)
}
