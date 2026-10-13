package cache

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestResourceKeyStringAndParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    ResourceKey
		wantErr bool
	}{
		{name: "core namespaced", in: "/ConfigMap/default/cm", want: ResourceKey{Kind: "ConfigMap", Namespace: "default", Name: "cm"}},
		{name: "grouped cluster scoped", in: "rbac.authorization.k8s.io/ClusterRole//admin", want: ResourceKey{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "admin"}},
		{name: "full", in: "apps/Deployment/ns/web", want: ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web"}},
		{name: "too few segments", in: "apps/Deployment/web", wantErr: true},
		{name: "too many segments", in: "apps/Deployment/ns/web/x", wantErr: true},
		{name: "missing kind", in: "apps//ns/web", wantErr: true},
		{name: "missing name", in: "apps/Deployment/ns/", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseResourceKey(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrInvalidResourceKey))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.in, got.String())
		})
	}
}

func segment(required bool) *rapid.Generator[string] {
	min := 0
	if required {
		min = 1
	}
	return rapid.StringOfN(rapid.RuneFrom([]rune("abcxyz.-0129AZ:")), min, 20, -1)
}

func TestResourceKeyRoundTripProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		k := ResourceKey{
			Group:     segment(false).Draw(t, "group"),
			Kind:      segment(true).Draw(t, "kind"),
			Namespace: segment(false).Draw(t, "namespace"),
			Name:      segment(true).Draw(t, "name"),
		}
		got, err := ParseResourceKey(k.String())
		if err != nil {
			t.Fatalf("parse %q: %v", k.String(), err)
		}
		if got != k {
			t.Fatalf("round trip mismatch: %#v != %#v", got, k)
		}
	})
}

func TestResourceKeyParseNeverPanicsProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		k, err := ParseResourceKey(s)
		if err == nil && k.String() != s {
			t.Fatalf("accepted %q but renders as %q", s, k.String())
		}
	})
}
