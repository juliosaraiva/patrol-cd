package notifications

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// maxSubscriptionsAnnotationBytes bounds the YAML subscriptions annotation we parse.
const maxSubscriptionsAnnotationBytes = 64 << 10

// Destinations maps a trigger name to the destinations subscribed to it.
type Destinations map[string][]services.Destination

// Add appends dest to trigger unless already present.
func (d Destinations) Add(trigger string, dest services.Destination) {
	for _, existing := range d[trigger] {
		if existing == dest {
			return
		}
	}
	d[trigger] = append(d[trigger], dest)
}

// Merge adds every destination of other.
func (d Destinations) Merge(other Destinations) {
	for t, dests := range other {
		for _, dest := range dests {
			d.Add(t, dest)
		}
	}
}

// Triggers returns the trigger names sorted.
func (d Destinations) Triggers() []string {
	out := make([]string, 0, len(d))
	for t := range d {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (d Destinations) sorted() {
	for t := range d {
		sort.SliceStable(d[t], func(i, j int) bool {
			if d[t][i].Service != d[t][j].Service {
				return d[t][i].Service < d[t][j].Service
			}
			return d[t][i].Recipient < d[t][j].Recipient
		})
	}
}

func triggersFor(service string, explicit, defaults []string, serviceDefaults map[string][]string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	if t, ok := serviceDefaults[service]; ok {
		return t
	}
	return defaults
}

// splitRecipients splits "a;b; c" into trimmed, non-empty recipients. An empty value
// yields a single empty recipient (services such as webhook and github need none).
func splitRecipients(v string) []string {
	var out []string
	for _, r := range strings.Split(v, ";") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

type annotationSubscription struct {
	Trigger      []string `json:"trigger"`
	Destinations []struct {
		Service    string   `json:"service"`
		Recipients []string `json:"recipients"`
	} `json:"destinations"`
}

// AnnotationDestinations computes the destinations declared by subscription annotations
// with the given prefix. Invalid entries are skipped and reported as warnings.
func AnnotationDestinations(prefix string, annotations map[string]string, defaultTriggers []string, serviceDefaults map[string][]string) (Destinations, []string) {
	dests := Destinations{}
	var warnings []string
	subscribe := SubscribeAnnotationPrefix(prefix)
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := annotations[k]
		switch {
		case strings.HasPrefix(k, subscribe):
			rest := strings.TrimPrefix(k, subscribe)
			trigger, service, hasTrigger := strings.Cut(rest, ".")
			if !hasTrigger {
				service, trigger = trigger, ""
			}
			if service == "" {
				warnings = append(warnings, fmt.Sprintf("annotation %q does not name a service", k))
				continue
			}
			var explicit []string
			if trigger != "" {
				explicit = []string{trigger}
			}
			for _, r := range splitRecipients(v) {
				for _, t := range triggersFor(service, explicit, defaultTriggers, serviceDefaults) {
					dests.Add(t, services.Destination{Service: service, Recipient: r})
				}
			}
		case k == SubscriptionsAnnotation(prefix):
			if len(v) > maxSubscriptionsAnnotationBytes {
				warnings = append(warnings, fmt.Sprintf("annotation %q exceeds %d bytes", k, maxSubscriptionsAnnotationBytes))
				continue
			}
			var subs []annotationSubscription
			if err := yaml.Unmarshal([]byte(v), &subs); err != nil {
				warnings = append(warnings, fmt.Sprintf("annotation %q is not a valid subscription list: %v", k, err))
				continue
			}
			for _, s := range subs {
				for _, d := range s.Destinations {
					if d.Service == "" {
						continue
					}
					recipients := d.Recipients
					if len(recipients) == 0 {
						recipients = []string{""}
					}
					for _, r := range recipients {
						for _, t := range triggersFor(d.Service, s.Trigger, defaultTriggers, serviceDefaults) {
							dests.Add(t, services.Destination{Service: d.Service, Recipient: strings.TrimSpace(r)})
						}
					}
				}
			}
		}
	}
	return dests, warnings
}

// LegacyRecipientsDestinations parses the deprecated recipients annotations: every key
// ending with suffix is a subscription whose trigger is the part before the suffix
// (trailing dots trimmed; empty means the default triggers) and whose value is a
// comma-separated list of "service[:recipient]" items. Only the text between the first and
// second ':' is the recipient. A service with service-specific default triggers always
// uses them, even when the key names a trigger (baseline behavior).
func LegacyRecipientsDestinations(suffix string, annotations map[string]string, defaultTriggers []string, serviceDefaults map[string][]string) Destinations {
	dests := Destinations{}
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		trigger := strings.TrimRight(k[:len(k)-len(suffix)], ".")
		triggers := defaultTriggers
		if trigger != "" {
			triggers = []string{trigger}
		}
		for _, item := range strings.Split(annotations[k], ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			parts := strings.Split(item, ":")
			d := services.Destination{Service: parts[0]}
			if len(parts) > 1 {
				d.Recipient = parts[1]
			}
			t := triggers
			if st, ok := serviceDefaults[d.Service]; ok {
				t = st
			}
			for _, name := range t {
				dests.Add(name, d)
			}
		}
	}
	return dests
}

// GlobalDestinations computes the destinations of the configured default subscriptions
// whose selector matches objLabels.
func (c *Config) GlobalDestinations(objLabels map[string]string) Destinations {
	dests := Destinations{}
	for _, s := range c.Subscriptions {
		if !s.Matches(objLabels) {
			continue
		}
		for _, r := range s.Recipients {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			service, recipient, _ := strings.Cut(r, ":")
			for _, t := range triggersFor(service, s.Triggers, c.DefaultTriggers, c.ServiceDefaultTriggers) {
				dests.Add(t, services.Destination{Service: service, Recipient: recipient})
			}
		}
	}
	return dests
}
