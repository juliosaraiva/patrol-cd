package settings

import (
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SourceState tells where a snapshot took the data of a settings object from.
type SourceState string

const (
	// SourceStateLoaded means the object was read.
	SourceStateLoaded SourceState = "loaded"
	// SourceStateRetained means the object is missing, or ignored for lacking the part-of
	// label, and the data last loaded from it is used instead. Only patrol-cm, patrol-rbac-cm
	// and patrol-secret are retained (see Parse).
	SourceStateRetained SourceState = "retained"
	// SourceStateAbsent means the object is missing or ignored and no earlier data is
	// available, so its keys use their defaults.
	SourceStateAbsent SourceState = "absent"
)

// retainedSources are the sources whose last loaded data outlives the object. Upstream keeps
// serving the last argocd-cm/argocd-secret settings when either disappears (GetSettings
// fails, so subscribers are not notified) and never unloads the RBAC policy (its informer has
// no delete handler). Falling back to defaults instead would re-enable a disabled admin, drop
// resource exclusions and deny rules, and clear webhook secrets (an empty one disables the
// signature check).
var retainedSources = []Source{SourceConfigMap, SourceRBACConfigMap, SourceSecret}

// retainedObject is the data last loaded from a settings object.
type retainedObject struct {
	name            string
	resourceVersion string
	data            map[string]string
	// refData is the object's data as a "$<secret>:<key>" target (nil unless it was one).
	refData map[string]string
}

// SourceState returns how the snapshot obtained the data of src. src must name one object
// (a ConfigMap source, SourceSecret or SourceNotificationsSecret); other values report
// SourceStateAbsent.
func (s *Settings) SourceState(src Source) SourceState {
	if st, ok := s.states[src]; ok {
		return st
	}
	return SourceStateAbsent
}

// secretStringData returns the decoded data of sec (StringData wins over Data).
func secretStringData(sec *corev1.Secret) map[string]string {
	data := make(map[string]string, len(sec.Data)+len(sec.StringData))
	for k, v := range sec.Data {
		data[k] = string(v)
	}
	for k, v := range sec.StringData {
		data[k] = v
	}
	return data
}

// retainUnavailable records the state of every named source and, for the retained sources
// whose object is unavailable, substitutes the data the previous snapshot loaded or
// retained. It runs at the end of index, before any source is parsed, so retained data goes
// through the same parsing (and per-key isolation) as live data.
func (p *parser) retainUnavailable() {
	for _, src := range configMapSources {
		p.s.states[src] = loadedOrAbsent(p.cms[src] != nil)
	}
	for _, src := range secretSources {
		p.s.states[src] = loadedOrAbsent(p.secrets[src] != nil)
	}
	for _, src := range retainedSources {
		if p.s.states[src] == SourceStateLoaded {
			p.s.retained[src] = p.loadedObject(src)
			continue
		}
		reason := p.unavailableReason(src)
		r, ok := p.previousObject(src)
		if !ok {
			switch src {
			case SourceConfigMap:
				p.errorf(src, "", "%s; local accounts are disabled and every other key uses its default", reason)
			case SourceSecret:
				p.errorf(src, "", "%s; local accounts are disabled", reason)
			}
			// patrol-rbac-cm is optional: without one the built-in policy applies.
			continue
		}
		p.restore(src, r)
		p.s.states[src] = SourceStateRetained
		p.s.retained[src] = r
		msg := reason + "; keeping the settings last loaded from it"
		if r.resourceVersion != "" {
			msg += fmt.Sprintf(" (resourceVersion %s)", r.resourceVersion)
		}
		msg += " until it is restored"
		if src == SourceConfigMap || src == SourceSecret {
			msg += "; local accounts are disabled meanwhile"
		}
		p.errorf(src, "", "%s", msg)
	}
}

func loadedOrAbsent(present bool) SourceState {
	if present {
		return SourceStateLoaded
	}
	return SourceStateAbsent
}

// loadedObject captures the data of a loaded source for the next snapshot.
func (p *parser) loadedObject(src Source) retainedObject {
	if cm := p.cms[src]; cm != nil {
		return retainedObject{name: cm.Name, resourceVersion: cm.ResourceVersion, data: p.cmData(src)}
	}
	sec := p.secrets[src]
	r := retainedObject{name: sec.Name, resourceVersion: sec.ResourceVersion, data: secretStringData(sec)}
	if d, ok := p.s.refSecrets[sec.Name]; ok {
		r.refData = d
	}
	return r
}

// unavailableReason says why src has no usable object.
func (p *parser) unavailableReason(src Source) string {
	if slices.Contains(secretSources, src) {
		return "Secret not found"
	}
	if p.ignored[src] {
		return fmt.Sprintf("ConfigMap is ignored because it is not labelled %s=%s", LabelKeyPartOf, LabelValuePartOf)
	}
	return "ConfigMap not found"
}

// previousObject returns the data the previous snapshot loaded or retained for src, when it
// describes the same object.
func (p *parser) previousObject(src Source) (retainedObject, bool) {
	if p.prev == nil || p.prev.namespace != p.in.Namespace {
		return retainedObject{}, false
	}
	r, ok := p.prev.retained[src]
	if !ok || r.name != p.object(src) {
		return retainedObject{}, false
	}
	return r, true
}

// restore puts an object holding r's data in place of the unavailable source src.
func (p *parser) restore(src Source, r retainedObject) {
	meta := metav1.ObjectMeta{Name: r.name, Namespace: p.in.Namespace, ResourceVersion: r.resourceVersion}
	if src == SourceSecret {
		data := make(map[string][]byte, len(r.data))
		for k, v := range r.data {
			data[k] = []byte(v)
		}
		p.secrets[src] = &corev1.Secret{ObjectMeta: meta, Data: data}
		if r.refData != nil {
			if _, live := p.s.refSecrets[r.name]; !live {
				p.s.refSecrets[r.name] = maps.Clone(r.refData)
			}
		}
		return
	}
	p.cms[src] = &corev1.ConfigMap{ObjectMeta: meta, Data: maps.Clone(r.data)}
}
