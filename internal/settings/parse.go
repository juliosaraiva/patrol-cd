package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Input is the raw material of a snapshot: the settings objects of the control-plane
// namespace. Objects are matched to sources by name; objects of other names are ignored,
// except Secrets labelled LabelKeyPartOf=LabelValuePartOf, which "$<secret>:<key>" references
// may point to.
type Input struct {
	Namespace string
	// Names selects the object names; empty fields select the defaults.
	Names      Names
	ConfigMaps []*corev1.ConfigMap
	Secrets    []*corev1.Secret
	// IgnorePartOfLabel disables the parity requirement that patrol-cm and the TLS, SSH and
	// GPG ConfigMaps (OPS-012) and referenced Secrets (TEN-131) carry
	// app.kubernetes.io/part-of=patrol-cd. Useful for offline validation of files.
	IgnorePartOfLabel bool
}

// ParseOption customizes Parse.
type ParseOption func(*parseOptions)

type parseOptions struct {
	previous *Settings
}

// WithPrevious makes Parse fall back, for every key whose new value cannot be parsed, to the
// value prev accepted for that key, and supplies the data prev loaded (or retained) for
// patrol-cm, patrol-rbac-cm and patrol-secret when those objects are unavailable.
func WithPrevious(prev *Settings) ParseOption {
	return func(o *parseOptions) { o.previous = prev }
}

// Parse builds a snapshot from in. It never fails: problems are reported as issues (see
// Settings.Validate) and the affected key falls back to the value the previous snapshot
// accepted (WithPrevious), or to its default.
//
// When patrol-cm, patrol-rbac-cm or patrol-secret is missing, or patrol-cm is ignored for
// lacking the part-of label, the data last loaded from that object (carried by prev) is parsed
// in its place and an error is reported (SourceStateRetained), as upstream keeps serving its
// last settings and RBAC policy then. While patrol-cm or patrol-secret is not loaded, every
// local account is disabled (upstream GetAccounts fails). Without earlier data the keys of
// the object use their defaults (SourceStateAbsent).
//
// Exceptions, which fail closed instead of keeping an older value:
//   - policy.csv and the policy.<name>.csv overlays are passed through unchanged, so an
//     invalid policy makes the RBAC enforcer deny everything until fixed (TEN-078);
//   - account settings are interpreted by auth.AccountsFromMaps, which disables an account
//     whose settings are malformed;
//   - invalid TLS certificate, SSH known host and GPG key entries are dropped, so no trust is
//     derived from them;
//   - invalid patrol-cmd-params-cm values select the compiled default (upstream semantics).
func Parse(in Input, opts ...ParseOption) *Settings {
	var o parseOptions
	for _, opt := range opts {
		opt(&o)
	}
	p := newParser(in, o.previous)
	p.run()
	return p.s
}

type parser struct {
	in      Input
	names   Names
	prev    *Settings
	s       *Settings
	issues  []Issue
	cms     map[Source]*corev1.ConfigMap
	secrets map[Source]*corev1.Secret
	// refSecretsUnlabelled are Secrets present in the input but not labelled; references to
	// them are reported, not resolved.
	refSecretsUnlabelled map[string]bool
	// ignored are the ConfigMap sources present in the input but ignored for their label.
	ignored map[Source]bool
}

func newParser(in Input, prev *Settings) *parser {
	names := in.Names.WithDefaults()
	return &parser{
		in:                   in,
		names:                names,
		prev:                 prev,
		s:                    newDefaultSettings(in.Namespace, names),
		cms:                  map[Source]*corev1.ConfigMap{},
		secrets:              map[Source]*corev1.Secret{},
		refSecretsUnlabelled: map[string]bool{},
		ignored:              map[Source]bool{},
	}
}

// newDefaultSettings returns a snapshot holding every default.
func newDefaultSettings(namespace string, names Names) *Settings {
	return &Settings{
		namespace:              namespace,
		names:                  names,
		resourceVersions:       map[string]string{},
		accepted:               map[Source]map[string]string{},
		states:                 map[Source]SourceState{},
		retained:               map[Source]retainedObject{},
		userSessionDuration:    DefaultUserSessionDuration,
		passwordPattern:        DefaultPasswordPattern,
		googleAnalytics:        GoogleAnalytics{AnonymizeUsers: true},
		resourceFilter:         &ResourcesFilter{},
		resourceOverrides:      map[string]luaOverride{},
		compareOptions:         defaultCompareOptions(),
		ignoreResourceUpdates:  true,
		enabledSourceTypes:     defaultEnabledSourceTypes(),
		helmValuesFileSchemes:  slices.Clone(DefaultHelmValuesFileSchemes),
		instanceLabelKey:       defaultInstanceLabelKey,
		trackingMethod:         defaultTrackingMethod,
		reconciliation:         Reconciliation{Timeout: DefaultReconciliationTimeout, Jitter: DefaultReconciliationJitter, HardTimeout: DefaultHardReconciliationTimeout},
		webhook:                Webhook{MaxPayloadSize: DefaultWebhookMaxPayloadSizeMB * 1024 * 1024, RefreshJitterThreshold: DefaultWebhookRefreshJitterThreshold},
		inClusterEnabled:       true,
		maxPodLogsToRender:     DefaultMaxPodLogsToRender,
		exec:                   Exec{Shells: slices.Clone(DefaultExecShells)},
		disableFineGrainedRBAC: true,
		deepLinks:              map[string][]DeepLink{},
		extensionConfigs:       map[string]string{},
		impersonation:          Impersonation{Enforced: true},
		hydrator:               Hydrator{CommitMessageTemplate: DefaultCommitMessageTemplate, ReadmeMessageTemplate: DefaultReadmeMessageTemplate},
		accounts:               defaultAccounts(),
		secretData:             map[string]string{},
		refSecrets:             map[string]map[string]string{},
		rbac:                   defaultRBAC(),
		tlsCerts:               map[string][]string{},
		gpgKeys:                map[string]GPGKey{},
		cmdParams:              map[string]string{},
	}
}

func (p *parser) run() {
	p.index()
	p.parseSecrets()
	p.parseConfigMap()
	p.parseAccounts()
	p.parseRBAC()
	p.parseCmdParams()
	p.parseTLSCerts()
	p.parseSSHKnownHosts()
	p.parseGPGKeys()
	p.parseNotifications()
	p.s.issues = sortIssues(p.issues)
	p.s.fingerprint = p.fingerprint()
}

// index assigns input objects to sources and applies the part-of label requirement.
func (p *parser) index() {
	if err := p.names.Validate(); err != nil {
		p.issues = append(p.issues, Issue{Severity: SeverityError, Message: "invalid object names: " + err.Error()})
	}
	byName := map[string]Source{}
	for _, src := range configMapSources {
		byName[p.names.Name(src)] = src
	}
	for _, cm := range p.in.ConfigMaps {
		if cm == nil || (p.in.Namespace != "" && cm.Namespace != "" && cm.Namespace != p.in.Namespace) {
			continue
		}
		src, ok := byName[cm.Name]
		if !ok {
			continue
		}
		if requiresPartOfLabel(src) && !p.in.IgnorePartOfLabel && cm.Labels[LabelKeyPartOf] != LabelValuePartOf {
			p.ignored[src] = true
			if !slices.Contains(retainedSources, src) {
				// Retained sources report it in retainUnavailable.
				p.addIssue(src, "", SeverityError, fmt.Sprintf("ConfigMap is ignored because it is not labelled %s=%s", LabelKeyPartOf, LabelValuePartOf))
			}
			continue
		}
		p.cms[src] = cm
		p.s.resourceVersions["configmap/"+cm.Name] = cm.ResourceVersion
	}
	secretByName := map[string]Source{p.names.Secret: SourceSecret, p.names.NotificationsSecret: SourceNotificationsSecret}
	for _, sec := range p.in.Secrets {
		if sec == nil || (p.in.Namespace != "" && sec.Namespace != "" && sec.Namespace != p.in.Namespace) {
			continue
		}
		if src, ok := secretByName[sec.Name]; ok {
			p.secrets[src] = sec
			p.s.resourceVersions["secret/"+sec.Name] = sec.ResourceVersion
		}
		if sec.Name == p.names.NotificationsSecret {
			// Notification credentials are never resolvable from patrol-cm references.
			continue
		}
		if !p.in.IgnorePartOfLabel && sec.Labels[LabelKeyPartOf] != LabelValuePartOf {
			p.refSecretsUnlabelled[sec.Name] = true
			continue
		}
		p.s.refSecrets[sec.Name] = secretStringData(sec)
		p.s.resourceVersions["secret/"+sec.Name] = sec.ResourceVersion
	}
	p.retainUnavailable()
}

// object returns the object name of src.
func (p *parser) object(src Source) string { return p.names.Name(src) }

func (p *parser) addIssue(src Source, key string, sev Severity, msg string) {
	p.issues = append(p.issues, Issue{Source: src, Object: p.object(src), Key: key, Severity: sev, Message: msg})
}

func (p *parser) warn(src Source, key, format string, args ...any) {
	p.addIssue(src, key, SeverityWarning, fmt.Sprintf(format, args...))
}

func (p *parser) errorf(src Source, key, format string, args ...any) {
	p.addIssue(src, key, SeverityError, fmt.Sprintf(format, args...))
}

// cmData returns a copy of the data of a ConfigMap source (nil when absent).
func (p *parser) cmData(src Source) map[string]string {
	cm := p.cms[src]
	if cm == nil {
		return nil
	}
	data := maps.Clone(cm.Data)
	if data == nil {
		data = map[string]string{}
	}
	for k, v := range cm.BinaryData {
		if _, dup := data[k]; dup {
			continue
		}
		data[k] = string(v)
	}
	return data
}

// accept records the raw value used for key.
func (p *parser) accept(src Source, key, value string) {
	m := p.s.accepted[src]
	if m == nil {
		m = map[string]string{}
		p.s.accepted[src] = m
	}
	m[key] = value
}

// previousValue returns the raw value the previous snapshot accepted for key.
func (p *parser) previousValue(src Source, key string) (string, bool) {
	if p.prev == nil {
		return "", false
	}
	v, ok := p.prev.accepted[src][key]
	return v, ok
}

// isolate applies one key with per-key isolation. apply must only change the snapshot when
// it returns nil. If it fails, an error issue is recorded and apply is retried with the value
// the previous snapshot accepted; when that is unavailable (or fails too) the default stays.
func (p *parser) isolate(src Source, key, value string, apply func(string) error) {
	err := apply(value)
	if err == nil {
		p.accept(src, key, value)
		return
	}
	if prevVal, ok := p.previousValue(src, key); ok && prevVal != value {
		if perr := apply(prevVal); perr == nil {
			p.accept(src, key, prevVal)
			p.errorf(src, key, "%v; keeping the last valid value", err)
			return
		}
	}
	p.errorf(src, key, "%v; using the default", err)
}

// fingerprint digests the input that influences the snapshot, including retained data and
// the state of every source.
func (p *parser) fingerprint() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, s := range parts {
			_, _ = fmt.Fprintf(h, "%d:%s", len(s), s) // hash.Hash writes never fail
		}
	}
	writeData := func(data map[string]string) {
		for _, k := range slices.Sorted(maps.Keys(data)) {
			write(k, data[k])
		}
	}
	write(p.in.Namespace, fmt.Sprint(p.names), fmt.Sprint(p.in.IgnorePartOfLabel))
	for _, src := range configMapSources {
		write(string(src), string(p.s.SourceState(src)), fmt.Sprint(p.ignored[src]))
		if cm := p.cms[src]; cm != nil {
			write(cm.Name)
			writeData(p.cmData(src))
		}
	}
	for _, src := range secretSources {
		write(string(src), string(p.s.SourceState(src)))
		if sec := p.secrets[src]; sec != nil {
			write(sec.Name, sec.Labels[LabelKeyPartOf])
			writeData(secretStringData(sec))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.s.refSecrets)) {
		write("ref", name)
		writeData(p.s.refSecrets[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// splitComma splits on commas without trimming (upstream strings.Split semantics).
func splitComma(v string) []string { return strings.Split(v, ",") }

// splitCommaNoSpaces removes every space and splits on commas, dropping empty items.
func splitCommaNoSpaces(v string) []string {
	v = strings.ReplaceAll(v, " ", "")
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}
