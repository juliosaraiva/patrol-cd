package settings

import (
	"maps"
	"regexp"
	"slices"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

var notificationsKeyRE = regexp.MustCompile(`key "([^"]+)"`)

// notificationsPlaceholder replaces Secret values during validation: the snapshot never
// holds notification credentials, only the Secret's key names.
const notificationsPlaceholder = "placeholder"

// parseNotifications records the notifications ConfigMap and the key names of the
// notifications Secret, and validates the configuration with internal/notifications.
func (p *parser) parseNotifications() {
	src := SourceNotificationsConfigMap
	n := Notifications{SecretName: p.names.NotificationsSecret}
	if sec := p.secrets[SourceNotificationsSecret]; sec != nil {
		n.SecretPresent = true
		keys := map[string]bool{}
		for k := range sec.Data {
			keys[k] = true
		}
		for k := range sec.StringData {
			keys[k] = true
		}
		n.SecretKeys = slices.Sorted(maps.Keys(keys))
	}
	data := p.cmData(src)
	n.ConfigMap = data
	p.s.notifications = n
	if data == nil {
		return
	}
	for k, v := range data {
		p.accept(src, k, v)
	}
	secret := make(map[string][]byte, len(n.SecretKeys))
	for _, k := range n.SecretKeys {
		secret[k] = []byte(notificationsPlaceholder)
	}
	cfg, err := notifications.ParseConfig(data, secret, notifications.WithDefaultCatalog())
	if err != nil {
		p.reportNotificationErrors(err)
		// Continue leniently to report the remaining cross-reference problems.
		cfg, err = notifications.ParseConfig(data, secret, notifications.WithDefaultCatalog(), notifications.Lenient())
		if err != nil {
			return
		}
	}
	for _, w := range cfg.Warnings {
		p.warn(src, notificationKey(w), "%s", w)
	}
	if err := cfg.Validate(); err != nil {
		p.reportNotificationErrors(err)
	}
}

func (p *parser) reportNotificationErrors(err error) {
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	for _, e := range errs {
		if e == nil {
			continue
		}
		p.errorf(SourceNotificationsConfigMap, notificationKey(e.Error()), "%s", e.Error())
	}
}

// notificationKey extracts the ConfigMap key named by a notifications error or warning.
func notificationKey(msg string) string {
	if m := notificationsKeyRE.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}
