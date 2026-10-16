package project

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

type dsa = v1alpha1.ApplicationDestinationServiceAccount

func saApp(appNS, destNS string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: appNS},
		Spec:       v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: "https://c1", Namespace: destNS}},
	}
}

// Ports the semantics of upstream TestDeriveServiceAccountMatchingNamespaces/Servers (TEN-029).
func TestDeriveServiceAccount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []dsa
		app     *v1alpha1.Application
		server  string
		want    string
		err     string
	}{
		{"no entries", nil, saApp("patrol", "web"), "https://c1", "", ""},
		{"exact", []dsa{{Server: "https://c1", Namespace: "web", DefaultServiceAccount: "deployer"}}, saApp("patrol", "web"), "https://c1", "system:serviceaccount:web:deployer", ""},
		{"qualified", []dsa{{Server: "https://c1", Namespace: "web", DefaultServiceAccount: "ops:deployer"}}, saApp("patrol", "web"), "https://c1", "system:serviceaccount:ops:deployer", ""},
		{"first match wins", []dsa{
			{Server: "https://c*", Namespace: "w*", DefaultServiceAccount: "first"},
			{Server: "https://c1", Namespace: "web", DefaultServiceAccount: "second"},
		}, saApp("patrol", "web"), "https://c1", "system:serviceaccount:web:first", ""},
		{"server mismatch", []dsa{{Server: "https://c2", Namespace: "*", DefaultServiceAccount: "sa"}}, saApp("patrol", "web"), "https://c1", "", ""},
		{"namespace mismatch", []dsa{{Server: "*", Namespace: "db", DefaultServiceAccount: "sa"}}, saApp("patrol", "web"), "https://c1", "", ""},
		{"glob set", []dsa{{Server: "*", Namespace: "{web,api}", DefaultServiceAccount: "sa"}}, saApp("patrol", "api"), "https://c1", "system:serviceaccount:api:sa", ""},
		// Empty destination namespace: matched against "", qualified with the app namespace.
		{"empty dest namespace", []dsa{{Server: "*", Namespace: "*", DefaultServiceAccount: "sa"}}, saApp("team-a", ""), "https://c1", "system:serviceaccount:team-a:sa", ""},
		{"empty namespace pattern", []dsa{{Server: "*", DefaultServiceAccount: "sa"}}, saApp("team-a", ""), "https://c1", "system:serviceaccount:team-a:sa", ""},
		{"invalid server glob", []dsa{{Server: "[[ech*", Namespace: "*", DefaultServiceAccount: "sa"}}, saApp("p", "web"), "https://c1", "", "invalid glob pattern for destination server: "},
		{"invalid namespace glob", []dsa{{Server: "*", Namespace: "[[ech*", DefaultServiceAccount: "sa"}}, saApp("p", "web"), "https://c1", "", "invalid glob pattern for destination namespace: "},
		{"invalid sa", []dsa{{Server: "*", Namespace: "*", DefaultServiceAccount: "test/sa"}}, saApp("p", "web"), "https://c1", "", "default service account contains invalid chars 'test/sa'"},
		{"blank sa", []dsa{{Server: "*", Namespace: "*", DefaultServiceAccount: "  "}}, saApp("p", "web"), "https://c1", "", "default service account contains invalid chars '  '"},
	}
	for _, tt := range tests {
		proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{DestinationServiceAccounts: tt.entries}}
		got, err := DeriveServiceAccount(proj, tt.app, tt.server)
		if tt.err != "" {
			require.Error(t, err, tt.name)
			assert.Contains(t, err.Error(), tt.err, tt.name)
			continue
		}
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// TEN-030 / SYNC-094.
func TestSelectServiceAccount(t *testing.T) {
	t.Parallel()
	matching := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{DestinationServiceAccounts: []dsa{{Server: "*", Namespace: "web", DefaultServiceAccount: "sa"}}}}
	empty := &v1alpha1.AppProject{}
	app := saApp("patrol", "web")

	imp, err := SelectServiceAccount(matching, app, "https://c1", ImpersonationSettings{Enabled: false, Enforced: true})
	require.NoError(t, err)
	assert.Empty(t, imp.UserName, "disabled: destinationServiceAccounts ignored")

	imp, err = SelectServiceAccount(matching, app, "https://c1", ImpersonationSettings{Enabled: true, Enforced: true})
	require.NoError(t, err)
	assert.Equal(t, "system:serviceaccount:web:sa", imp.UserName)

	_, err = SelectServiceAccount(empty, app, "https://c1", ImpersonationSettings{Enabled: true, Enforced: true})
	require.Error(t, err)
	assert.EqualError(t, err, "no matching service account found for destination server https://c1 and namespace web")
	var nsa *NoServiceAccountError
	assert.True(t, errors.As(err, &nsa))

	imp, err = SelectServiceAccount(empty, app, "https://c1", ImpersonationSettings{Enabled: true, Enforced: false})
	require.NoError(t, err)
	assert.Empty(t, imp.UserName, "not enforced: controller identity")

	tests := []struct {
		data map[string]string
		want ImpersonationSettings
	}{
		{nil, ImpersonationSettings{Enabled: false, Enforced: true}},
		{map[string]string{SettingImpersonationEnabled: "true"}, ImpersonationSettings{Enabled: true, Enforced: true}},
		{map[string]string{SettingImpersonationEnabled: "True"}, ImpersonationSettings{Enabled: false, Enforced: true}},
		{map[string]string{SettingImpersonationEnabled: "true", SettingImpersonationEnforced: "false"}, ImpersonationSettings{Enabled: true, Enforced: false}},
		{map[string]string{SettingImpersonationEnforced: "garbage"}, ImpersonationSettings{Enabled: false, Enforced: true}},
		{map[string]string{SettingImpersonationEnforced: "FALSE"}, ImpersonationSettings{Enabled: false, Enforced: true}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ImpersonationSettingsFromConfigMap(tt.data), "%v", tt.data)
	}
}
