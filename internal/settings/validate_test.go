package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateGroups(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		KeyDexConfig:             "connectors: []",
		KeyKustomizeBuildOptions: "--enable-helm",
		"kustomize.path.v1":      "/k1",
		"kustomize.path.v2":      "/k2",
		"accounts.alice":         "login",
		"resource.customizations.health.apps_Deployment": "return {}",
		KeyResourceRespectRBAC:                           "bad",
	}, newCM(DefaultRBACConfigMapName, map[string]string{RBACKeyPolicyDefault: "role:readonly"})))
	res, err := ValidateGroups(s)
	require.NoError(t, err)
	byName := map[string]GroupResult{}
	for _, r := range res {
		byName[r.Group] = r
	}
	require.Len(t, byName, len(ValidationGroups()))
	assert.Equal(t, "Dex is configured ('url' field is missing)", byName[GroupGeneral].Summary)
	assert.False(t, byName[GroupGeneral].OK, "resource.respectRBAC error belongs to general")
	assert.Equal(t, "--enable-helm (2 versions)", byName[GroupKustomize].Summary)
	assert.True(t, byName[GroupKustomize].OK)
	assert.Equal(t, "2 accounts", byName[GroupAccounts].Summary)
	assert.Equal(t, "2 resource overrides", byName[GroupResourceOverrides].Summary, "the */* status override counts, as upstream")
	assert.Contains(t, byName[GroupRBAC].Summary, `default role "role:readonly"`)

	only, err := ValidateGroups(s, GroupKustomize)
	require.NoError(t, err)
	require.Len(t, only, 1)
	_, err = ValidateGroups(s, "plugins")
	assert.ErrorContains(t, err, `unknown validation group "plugins"`)

	none := Parse(in(map[string]string{}))
	res, err = ValidateGroups(none, GroupGeneral, GroupKustomize)
	require.NoError(t, err)
	assert.Equal(t, "SSO is not configured", res[0].Summary)
	assert.Equal(t, "default options", res[1].Summary)
}
