package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNotificationsSettings(t *testing.T) {
	t.Parallel()
	cm := newCM(DefaultNotificationsConfigMapName, map[string]string{
		"service.slack":        "token: $slack-token\n",
		"template.my-template": "message: hello {{.app.metadata.name}}\n",
		"trigger.on-custom":    "- when: app.status.sync.status == 'Synced'\n  send: [my-template]\n",
		"trigger.on-broken":    "- when: app.status.sync.status ==\n  send: [missing-template]\n",
		"defaultTriggers":      "[on-custom]\n",
		"bogus.key":            "x",
	})
	cm.Labels = nil // read by name
	sec := newSecret(DefaultNotificationsSecretName, map[string]string{"slack-token": "xoxb-not-a-real-token"}, false)
	s := Parse(in(map[string]string{}, cm, sec))
	n := s.Notifications()
	assert.True(t, n.SecretPresent)
	assert.Equal(t, DefaultNotificationsSecretName, n.SecretName)
	assert.Equal(t, []string{"slack-token"}, n.SecretKeys, "only key names are kept")
	assert.Equal(t, cm.Data, n.ConfigMap)
	requireIssue(t, s, DefaultNotificationsConfigMapName, "bogus.key", SeverityWarning, "is not supported")
	requireIssue(t, s, DefaultNotificationsConfigMapName, "", SeverityError, `trigger "on-broken" sends undefined template "missing-template"`)
	for _, is := range s.Validate() {
		assert.NotContains(t, is.Message, "xoxb-not-a-real-token")
	}
	_, err := s.ResolveSecretRef("$" + DefaultNotificationsSecretName + ":slack-token")
	assert.ErrorIs(t, err, ErrSecretNotFound, "notification credentials are never resolvable from patrol-cm")
}

func TestNotificationsAbsent(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{}))
	n := s.Notifications()
	assert.Nil(t, n.ConfigMap)
	assert.False(t, n.SecretPresent)
	assert.Empty(t, issuesFor(s, DefaultNotificationsConfigMapName, ""))
}
