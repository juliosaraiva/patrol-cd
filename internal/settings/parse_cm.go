package settings

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
)

const (
	defaultInstanceLabelKey = v1alpha1.LabelKeyAppInstanceDefault
	defaultTrackingMethod   = v1alpha1.TrackingMethodAnnotation
)

// DefaultCommitMessageTemplate is the source hydrator commit message template used when
// sourceHydrator.commitMessageTemplate is unset (upstream default, data reused for parity).
const DefaultCommitMessageTemplate = `{{.metadata.drySha | trunc 7}}: {{ .metadata.subject }}
{{- if .metadata.body }}

{{ .metadata.body }}
{{- end }}
{{ range $ref := .metadata.references }}
{{- if and $ref.commit $ref.commit.author }}
Co-authored-by: {{ $ref.commit.author }}
{{- end }}
{{- end }}
{{- if .metadata.author }}
Co-authored-by: {{ .metadata.author }}
{{- end }}
`

// DefaultReadmeMessageTemplate is the hydrated README template used when
// sourceHydrator.readmeMessageTemplate is unset (upstream default, data reused for parity).
const DefaultReadmeMessageTemplate = "# Manifest Hydration\n\n" +
	"To hydrate the manifests in this repository, run the following commands:\n\n" +
	"```shell\n" +
	"git clone {{ .RepoURL }}\n" +
	"# cd into the cloned directory\n" +
	"git checkout {{ .DrySHA }}\n" +
	"{{ range $command := .Commands -}}\n" +
	"{{ $command }}\n" +
	"{{ end -}}```\n" +
	"{{ if .References -}}\n\n" +
	"## References\n\n" +
	"{{ range $ref := .References -}}\n" +
	"{{ if $ref.Commit -}}\n" +
	"* [{{ $ref.Commit.SHA | mustRegexFind \"[0-9a-f]+\" | trunc 7 }}]({{ $ref.Commit.RepoURL }}): {{ $ref.Commit.Subject }} ({{ $ref.Commit.Author }})\n" +
	"{{ end -}}\n" +
	"{{ end -}}\n" +
	"{{ end -}}"

func defaultEnabledSourceTypes() map[v1alpha1.ApplicationSourceType]bool {
	return map[v1alpha1.ApplicationSourceType]bool{
		v1alpha1.ApplicationSourceTypeKustomize: true,
		v1alpha1.ApplicationSourceTypeHelm:      true,
		v1alpha1.ApplicationSourceTypeDirectory: true,
		v1alpha1.ApplicationSourceTypePlugin:    true,
	}
}

// ValidateExternalURL accepts "" and absolute http(s) URLs (upstream wording).
func ValidateExternalURL(u string) error {
	if u == "" {
		return nil
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("failed to parse URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("URL must include http or https protocol")
	}
	return nil
}

// upstreamErrorf builds an error whose text reproduces an upstream message verbatim, including
// its leading capital letter; such texts become user-facing Issue messages, so the Go
// convention for error strings does not apply to them.
func upstreamErrorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// cmKeys tracks which patrol-cm keys were consumed.
type cmKeys struct {
	p       *parser
	data    map[string]string
	handled map[string]bool
}

// get marks key handled and returns its value.
func (c *cmKeys) get(key string) (string, bool) {
	v, ok := c.data[key]
	if ok {
		c.handled[key] = true
	}
	return v, ok
}

// apply runs fn with per-key isolation when key is present.
func (c *cmKeys) apply(key string, fn func(string) error) {
	if v, ok := c.get(key); ok {
		c.p.isolate(SourceConfigMap, key, v, fn)
	}
}

// prefixed returns the present keys starting with prefix, sorted, and marks them handled.
func (c *cmKeys) prefixed(prefix string) []string {
	var keys []string
	for k := range c.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
			c.handled[k] = true
		}
	}
	slices.Sort(keys)
	return keys
}

// boolTrue interprets v like upstream's `== "true"`, warning about non-canonical values.
func (p *parser) boolTrue(key, v string) bool {
	switch v {
	case "true":
		return true
	case "false", "":
		return false
	}
	p.warn(SourceConfigMap, key, `value %q is treated as false; only "true" enables it`, v)
	return false
}

// boolNotFalse interprets v like upstream's `!= "false"`, warning about non-canonical values.
func (p *parser) boolNotFalse(key, v string) bool {
	switch v {
	case "false":
		return false
	case "true", "":
		return true
	}
	p.warn(SourceConfigMap, key, `value %q is treated as true; only "false" disables it`, v)
	return true
}

func (p *parser) parseConfigMap() {
	src := SourceConfigMap
	data := p.cmData(src)
	if data == nil {
		// Reported by retainUnavailable.
		data = map[string]string{}
	}
	c := &cmKeys{p: p, data: data, handled: map[string]bool{}}
	s := p.s

	// Accounts are interpreted by auth.AccountsFromMaps (parseAccounts).
	c.prefixed(KeyAccountsPrefix)
	c.get(KeyAdminEnabled)

	c.apply(KeyURL, func(v string) error {
		if err := ValidateExternalURL(v); err != nil {
			p.warn(src, KeyURL, "Failed to validate URL in configmap: %v", err)
		}
		s.url = v
		return nil
	})
	c.apply(KeyAdditionalURLs, func(v string) error {
		var urls []string
		if v != "" {
			if err := yaml.Unmarshal([]byte(v), &urls); err != nil {
				return upstreamErrorf("Failed to decode all additional URLs in configmap: %w", err)
			}
		}
		for _, u := range urls {
			if err := ValidateExternalURL(u); err != nil {
				p.warn(src, KeyAdditionalURLs, "Failed to validate external URL in configmap: %v", err)
			}
		}
		s.additionalURLs = urls
		return nil
	})
	c.apply(KeyStatusBadgeEnabled, func(v string) error { s.statusBadge.Enabled = p.boolTrue(KeyStatusBadgeEnabled, v); return nil })
	c.apply(KeyStatusBadgeURL, func(v string) error {
		if err := ValidateExternalURL(v); err != nil {
			p.warn(src, KeyStatusBadgeURL, "Failed to validate status badge URL in configmap: %v", err)
		}
		s.statusBadge.RootURL = v
		return nil
	})
	c.apply(KeyAnonymousUserEnabled, func(v string) error { s.anonymousUserEnabled = p.boolTrue(KeyAnonymousUserEnabled, v); return nil })
	c.apply(KeyUserSessionDuration, func(v string) error {
		d, err := auth.ParseSessionDuration(v)
		if err != nil {
			return upstreamErrorf("Failed to parse '%s' key: %w", KeyUserSessionDuration, err)
		}
		s.userSessionDuration = d
		return nil
	})
	c.apply(KeyPasswordPattern, func(v string) error {
		if v == "" {
			s.passwordPattern = DefaultPasswordPattern
			return nil
		}
		if _, err := regexp.Compile(v); err != nil {
			return fmt.Errorf("failed to compile password regex: %w", err)
		}
		s.passwordPattern = v
		return nil
	})

	// Google Analytics and help.
	c.apply(KeyGATrackingID, func(v string) error { s.googleAnalytics.TrackingID = v; return nil })
	c.apply(KeyGAAnonymizeUsers, func(v string) error {
		s.googleAnalytics.AnonymizeUsers = p.boolNotFalse(KeyGAAnonymizeUsers, v)
		return nil
	})
	chatURL, hasChatURL := c.get(KeyHelpChatURL)
	chatText, hasChatText := c.get(KeyHelpChatText)
	if !hasChatText {
		chatText = DefaultHelpChatText
	}
	if !hasChatURL {
		chatText = ""
	}
	s.help.ChatURL, s.help.ChatText = chatURL, chatText
	if hasChatURL {
		p.accept(src, KeyHelpChatURL, chatURL)
	}
	if hasChatText {
		p.accept(src, KeyHelpChatText, data[KeyHelpChatText])
	}
	s.help.BinaryURLs = map[string]string{}
	for _, k := range c.prefixed(KeyHelpDownloadPrefix) {
		platform := strings.TrimPrefix(k, KeyHelpDownloadPrefix)
		if !slices.Contains(helpDownloadPlatforms, platform) {
			p.warn(src, k, "unknown platform %q is ignored; supported: %s", platform, strings.Join(helpDownloadPlatforms, ", "))
			continue
		}
		s.help.BinaryURLs[platform] = data[k]
		p.accept(src, k, data[k])
	}

	// SSO.
	p.parseDex(c)
	p.parseOIDC(c)
	c.apply(KeyOIDCTLSInsecureSkip, func(v string) error { s.oidcTLSInsecureSkip = p.boolTrue(KeyOIDCTLSInsecureSkip, v); return nil })

	// Resources.
	p.parseCustomizations(c)
	c.apply(KeyResourceExclusions, func(v string) error {
		rs, err := p.filteredResources(KeyResourceExclusions, v)
		if err != nil {
			return fmt.Errorf("error unmarshalling excluded resources %w", err)
		}
		s.resourceFilter.ResourceExclusions = rs
		return nil
	})
	c.apply(KeyResourceInclusions, func(v string) error {
		rs, err := p.filteredResources(KeyResourceInclusions, v)
		if err != nil {
			return fmt.Errorf("error unmarshalling included resources %w", err)
		}
		s.resourceFilter.ResourceInclusions = rs
		return nil
	})
	c.apply(KeyResourceCompareOptions, func(v string) error {
		var doc compareOptionsDoc
		if err := yaml.Unmarshal([]byte(v), &doc); err != nil {
			return upstreamErrorf("Could not get compare options from ConfigMap: %w", err)
		}
		opts := diff.DefaultCompareOptions()
		opts.IgnoreAggregatedRoles = doc.IgnoreAggregatedRoles
		if doc.IgnoreDifferencesOnResourceUpdates != nil {
			opts.IgnoreDifferencesOnResourceUpdates = *doc.IgnoreDifferencesOnResourceUpdates
		}
		mode, ok := diff.ParseIgnoreStatusMode(doc.IgnoreResourceStatusField)
		if !ok {
			p.warn(src, KeyResourceCompareOptions, "Unrecognized value for ignoreResourceStatusField - %s, ignore status for all resources", doc.IgnoreResourceStatusField)
		}
		opts.IgnoreResourceStatusField = mode
		s.compareOptions = opts
		return nil
	})
	c.apply(KeyResourceRespectRBAC, func(v string) error {
		switch v {
		case "", "normal", "strict":
			s.respectRBAC = v
			return nil
		}
		return fmt.Errorf("invalid value for %s: %s", KeyResourceRespectRBAC, v)
	})
	c.apply(KeyIgnoreResourceUpdates, func(v string) error {
		if v == "" {
			s.ignoreResourceUpdates = true
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("error parsing %s: %w", KeyIgnoreResourceUpdates, err)
		}
		s.ignoreResourceUpdates = b
		return nil
	})
	c.apply(KeySensitiveMaskAnnots, func(v string) error { s.sensitiveMaskAnnots = splitCommaNoSpaces(v); return nil })
	c.apply(KeyResourceCustomLabels, func(v string) error {
		s.customLabels = nil
		if v != "" {
			s.customLabels = splitComma(v)
			p.warnUntrimmed(KeyResourceCustomLabels, s.customLabels)
		}
		return nil
	})
	c.apply(KeyIncludeEventLabelKeys, func(v string) error {
		s.includeEventLabelKeys = splitCommaNoSpaces(v)
		p.checkGlobs(KeyIncludeEventLabelKeys, s.includeEventLabelKeys)
		return nil
	})
	c.apply(KeyExcludeEventLabelKeys, func(v string) error {
		s.excludeEventLabelKeys = splitCommaNoSpaces(v)
		p.checkGlobs(KeyExcludeEventLabelKeys, s.excludeEventLabelKeys)
		return nil
	})

	// Rendering tools.
	for key, typ := range map[string]v1alpha1.ApplicationSourceType{
		KeyKustomizeEnable: v1alpha1.ApplicationSourceTypeKustomize,
		KeyHelmEnable:      v1alpha1.ApplicationSourceTypeHelm,
		KeyJsonnetEnable:   v1alpha1.ApplicationSourceTypeDirectory,
	} {
		c.apply(key, func(v string) error {
			if v != "" {
				s.enabledSourceTypes[typ] = p.boolTrue(key, v)
			}
			return nil
		})
	}
	p.parseKustomize(c)
	if v, ok := c.get(KeyHelmValuesFileSchemes); ok {
		var schemes []string
		for item := range strings.SplitSeq(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				schemes = append(schemes, item)
			}
		}
		s.helmValuesFileSchemes = schemes
		p.accept(src, KeyHelmValuesFileSchemes, v)
	}

	// Tracking.
	c.apply(KeyInstanceLabelKey, func(v string) error {
		if v == "" {
			s.instanceLabelKey = defaultInstanceLabelKey
			return nil
		}
		if errs := validation.IsQualifiedName(v); len(errs) > 0 {
			return fmt.Errorf("invalid label key %q: %s", v, strings.Join(errs, "; "))
		}
		s.instanceLabelKey = v
		return nil
	})
	c.apply(KeyTrackingMethod, func(v string) error {
		switch m := v1alpha1.TrackingMethod(v); m {
		case "":
			s.trackingMethod = defaultTrackingMethod
		case v1alpha1.TrackingMethodAnnotation, v1alpha1.TrackingMethodLabel, v1alpha1.TrackingMethodAnnotationAndLabel:
			s.trackingMethod = m
		default:
			return fmt.Errorf("invalid tracking method %q: must be %s, %s or %s", v, v1alpha1.TrackingMethodAnnotation, v1alpha1.TrackingMethodLabel, v1alpha1.TrackingMethodAnnotationAndLabel)
		}
		return nil
	})
	c.apply(KeyAllowedNodeLabels, func(v string) error {
		var keys []string
		for _, k := range splitCommaNoSpaces(v) {
			if errs := validation.IsQualifiedName(k); len(errs) > 0 {
				p.warn(src, KeyAllowedNodeLabels, "Invalid node label key '%s' in configmap: %v", k, errs)
				continue
			}
			keys = append(keys, k)
		}
		s.allowedNodeLabels = keys
		return nil
	})
	c.apply(KeyInstallationID, func(v string) error { s.installationID = v; return nil })

	// UI.
	c.apply(KeyUICSSURL, func(v string) error { s.ui.CSSURL = v; return nil })
	c.apply(KeyUIBannerContent, func(v string) error { s.ui.BannerContent = v; return nil })
	c.apply(KeyUIBannerURL, func(v string) error {
		if err := ValidateExternalURL(v); err != nil {
			p.warn(src, KeyUIBannerURL, "Failed to validate UI banner URL in configmap: %v", err)
		}
		s.ui.BannerURL = v
		return nil
	})
	c.apply(KeyUIBannerPermanent, func(v string) error { s.ui.BannerPermanent = p.boolTrue(KeyUIBannerPermanent, v); return nil })
	c.apply(KeyUIBannerPosition, func(v string) error {
		switch v {
		case "", "top", "bottom", "both":
		default:
			p.warn(src, KeyUIBannerPosition, `unknown banner position %q; valid values are "top", "bottom" and "both"`, v)
		}
		s.ui.BannerPosition = v
		return nil
	})

	// Reconciliation.
	c.apply(KeyReconciliationTimeout, func(v string) error {
		d, err := nonNegativeDuration(v)
		if err != nil {
			return err
		}
		s.reconciliation.Timeout, s.reconciliation.TimeoutSet = d, v != ""
		if v == "" {
			s.reconciliation.Timeout = DefaultReconciliationTimeout
		}
		return nil
	})
	c.apply(KeyReconciliationJitter, func(v string) error {
		d, err := nonNegativeDuration(v)
		if err != nil {
			return err
		}
		if v == "" {
			d = DefaultReconciliationJitter
		}
		s.reconciliation.Jitter = d
		return nil
	})
	c.apply(KeyHardReconciliation, func(v string) error {
		d, err := nonNegativeDuration(v)
		if err != nil {
			return err
		}
		s.reconciliation.HardTimeout = d
		return nil
	})

	// Webhooks (secrets are resolved by parseSecrets).
	c.apply(KeyWebhookMaxPayloadSizeMB, func(v string) error {
		if v == "" {
			s.webhook.MaxPayloadSize = DefaultWebhookMaxPayloadSizeMB * 1024 * 1024
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return upstreamErrorf("Failed to parse '%s' key: %w", KeyWebhookMaxPayloadSizeMB, err)
		}
		if n < 0 || n > (1<<63-1)/(1024*1024) {
			return upstreamErrorf("Failed to parse '%s' key: value %d out of range", KeyWebhookMaxPayloadSizeMB, n)
		}
		s.webhook.MaxPayloadSize = n * 1024 * 1024
		return nil
	})
	c.apply(KeyWebhookRefreshJitter, func(v string) error {
		d, err := nonNegativeDuration(v)
		if err != nil {
			return upstreamErrorf("Failed to parse '%s' key: %w", KeyWebhookRefreshJitter, err)
		}
		s.webhook.RefreshJitter = d
		return nil
	})
	c.apply(KeyWebhookJitterThreshold, func(v string) error {
		if v == "" {
			s.webhook.RefreshJitterThreshold = DefaultWebhookRefreshJitterThreshold
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return upstreamErrorf("Failed to parse '%s' key: %w", KeyWebhookJitterThreshold, err)
		}
		if n < 0 {
			return upstreamErrorf("Invalid '%s' value %d, must be >= 0", KeyWebhookJitterThreshold, n)
		}
		s.webhook.RefreshJitterThreshold = n
		return nil
	})

	// Clusters, server, exec.
	c.apply(KeyInClusterEnabled, func(v string) error { s.inClusterEnabled = p.boolNotFalse(KeyInClusterEnabled, v); return nil })
	c.apply(KeyMaxPodLogsToRender, func(v string) error {
		if v == "" {
			s.maxPodLogsToRender = DefaultMaxPodLogsToRender
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return upstreamErrorf("Failed to parse '%s' key: %w", KeyMaxPodLogsToRender, err)
		}
		if n < 0 {
			return upstreamErrorf("Failed to parse '%s' key: value %d must not be negative", KeyMaxPodLogsToRender, n)
		}
		s.maxPodLogsToRender = n
		return nil
	})
	c.apply(KeyExecEnabled, func(v string) error { s.exec.Enabled = p.boolTrue(KeyExecEnabled, v); return nil })
	c.apply(KeyExecShells, func(v string) error {
		if v == "" {
			s.exec.Shells = slices.Clone(DefaultExecShells)
			return nil
		}
		s.exec.Shells = splitComma(v)
		p.warnUntrimmed(KeyExecShells, s.exec.Shells)
		return nil
	})
	c.apply(KeyDisableFineGrainedRBAC, func(v string) error {
		if v == "" {
			s.disableFineGrainedRBAC = true
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("error parsing %s: %w", KeyDisableFineGrainedRBAC, err)
		}
		s.disableFineGrainedRBAC = b
		return nil
	})

	// Deep links, extensions, global projects.
	for _, key := range []string{KeyApplicationLinks, KeyResourceLinks, KeyProjectLinks} {
		c.apply(key, func(v string) error {
			links, err := p.parseDeepLinks(key, v)
			if err != nil {
				return err
			}
			s.deepLinks[key] = links
			return nil
		})
	}
	p.parseExtensions(c)
	c.apply(KeyGlobalProjects, func(v string) error {
		gps, err := p.parseGlobalProjects(v)
		if err != nil {
			return err
		}
		s.globalProjects = gps
		return nil
	})

	// Sync.
	c.apply(KeyImpersonationEnabled, func(v string) error { s.impersonation.Enabled = p.boolTrue(KeyImpersonationEnabled, v); return nil })
	c.apply(KeyImpersonationEnforced, func(v string) error {
		s.impersonation.Enforced = p.boolNotFalse(KeyImpersonationEnforced, v)
		return nil
	})
	c.apply(KeyRequireOverrideRevision, func(v string) error {
		if v == "" {
			s.requireOverrideRevision = false
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("error parsing %s value: %w, expected true or false", KeyRequireOverrideRevision, err)
		}
		s.requireOverrideRevision = b
		return nil
	})

	// Source hydrator.
	c.apply(KeyCommitAuthorName, func(v string) error { s.hydrator.CommitAuthorName = v; return nil })
	c.apply(KeyCommitAuthorEmail, func(v string) error {
		if v != "" && !strings.Contains(v, "@") {
			p.warn(src, KeyCommitAuthorEmail, "%q does not look like an email address", v)
		}
		s.hydrator.CommitAuthorEmail = v
		return nil
	})
	c.apply(KeyCommitMessageTemplate, func(v string) error {
		if v == "" {
			s.hydrator.CommitMessageTemplate = DefaultCommitMessageTemplate
			return nil
		}
		if err := checkTemplate(v); err != nil {
			return fmt.Errorf("invalid commit message template: %w", err)
		}
		s.hydrator.CommitMessageTemplate = v
		return nil
	})
	c.apply(KeyReadmeMessageTemplate, func(v string) error {
		if v == "" {
			s.hydrator.ReadmeMessageTemplate = DefaultReadmeMessageTemplate
			return nil
		}
		if err := checkTemplate(v); err != nil {
			return fmt.Errorf("invalid README template: %w", err)
		}
		s.hydrator.ReadmeMessageTemplate = v
		return nil
	})

	// Remaining keys: deprecated, unknown or misplaced.
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if c.handled[k] {
			continue
		}
		spec, ok := LookupKey(src, k)
		switch {
		case ok && spec.Deprecated != "":
			p.warn(src, k, "key is ignored: %s", spec.Deprecated)
		case ok:
			p.warn(src, k, "key is accepted but was not interpreted (internal error)")
		default:
			p.unknownKey(src, k)
		}
	}
}

// unknownKey reports an unknown key, suggesting a likely intended key.
func (p *parser) unknownKey(src Source, key string) {
	if s, ok := keySuggestions[key]; ok {
		p.warn(src, key, "unknown key is ignored; did you mean %q?", s)
		return
	}
	if _, ok := LookupKey(SourceCmdParamsConfigMap, key); ok && src == SourceConfigMap {
		p.warn(src, key, "unknown key is ignored; %q is a %s key", key, p.object(SourceCmdParamsConfigMap))
		return
	}
	p.warn(src, key, "unknown key is ignored")
}

func (p *parser) warnUntrimmed(key string, items []string) {
	for _, it := range items {
		if strings.TrimSpace(it) != it {
			p.warn(SourceConfigMap, key, "item %q contains surrounding whitespace, which is not trimmed", it)
		}
	}
}

func (p *parser) checkGlobs(key string, patterns []string) {
	for _, pat := range patterns {
		if _, err := glob.Compile(pat); err != nil {
			p.warn(SourceConfigMap, key, "invalid glob pattern %q never matches: %v", pat, err)
		}
	}
}

func (p *parser) filteredResources(key, v string) ([]FilteredResource, error) {
	rs, err := parseFilteredResources(v)
	if err != nil {
		return nil, err
	}
	for i := range rs {
		for _, msg := range rs[i].compile() {
			p.warn(SourceConfigMap, key, "entry %d: %s; the pattern never matches", i, msg)
		}
	}
	return rs, nil
}

func nonNegativeDuration(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := parseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", v)
	}
	return d, nil
}

// parseCustomizations applies resource.customizations and resource.customizations.* with
// per-key isolation, delegating the parsing to lua.ParseResourceOverrides.
func (p *parser) parseCustomizations(c *cmKeys) {
	src := SourceConfigMap
	effective := map[string]string{}
	keys := c.prefixed(KeyResourceCustomizations)
	for _, k := range keys {
		if k != KeyResourceCustomizations && !strings.HasPrefix(k, KeyResourceCustomizations+".") {
			c.handled[k] = false
			continue
		}
		if k != KeyResourceCustomizations && strings.Count(k, ".") < 3 {
			p.warn(src, k, "key is ignored: expected resource.customizations.<type>.<group_kind>")
			continue
		}
		p.isolate(src, k, c.data[k], func(v string) error {
			if _, err := lua.ParseResourceOverrides(map[string]string{k: v}); err != nil {
				return err
			}
			effective[k] = v
			return nil
		})
	}
	overrides, err := lua.ParseResourceOverrides(effective)
	if err != nil {
		p.errorf(src, KeyResourceCustomizations, "%v; resource customizations are not applied", err)
		overrides = map[string]lua.ResourceOverride{}
	}
	supported := diff.SupportedKnownTypes()
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		for _, f := range overrides[key].KnownTypeFields {
			if !slices.Contains(supported, f.Type) {
				p.warn(src, KeyResourceCustomizations, "knownTypeFields of %q: unsupported type %q for field %q is skipped", key, f.Type, f.Field)
			}
		}
	}
	p.s.resourceOverrides = overrides
}

// parseKustomize builds the version registry from kustomize.path.<v>, the deprecated
// kustomize.version.<v>, and kustomize.buildOptions[.<v>].
func (p *parser) parseKustomize(c *cmKeys) {
	src := SourceConfigMap
	s := p.s
	if v, ok := c.get(KeyKustomizeBuildOptions); ok {
		s.kustomize.BuildOptions = v
		p.accept(src, KeyKustomizeBuildOptions, v)
	}
	versions := map[string]KustomizeVersion{}
	// kustomize.path.* sorts before kustomize.version.*, so the path form wins a duplicate.
	for _, prefix := range []string{KeyKustomizePathPrefix, KeyKustomizeVersionPrefix} {
		for _, k := range c.prefixed(prefix) {
			name := strings.TrimPrefix(k, prefix)
			if prefix == KeyKustomizeVersionPrefix {
				p.warn(src, k, "kustomize.version.<version> is deprecated; use kustomize.path.<version>")
			}
			if name == "" {
				p.errorf(src, k, "missing version name; the key is ignored")
				continue
			}
			if _, dup := versions[name]; dup {
				p.errorf(src, k, "failed to add kustomize version from %q: found duplicate kustomize version: %s", k, name)
				continue
			}
			versions[name] = KustomizeVersion{Name: name, Path: c.data[k]}
			p.accept(src, k, c.data[k])
		}
	}
	for _, k := range c.prefixed(KeyKustomizeBuildOptions + ".") {
		name := strings.TrimPrefix(k, KeyKustomizeBuildOptions+".")
		v, ok := versions[name]
		if !ok {
			p.warn(src, k, "build options for kustomize version %q are ignored: the version is not registered with kustomize.path.%s", name, name)
			continue
		}
		v.BuildOptions = c.data[k]
		versions[name] = v
		p.accept(src, k, c.data[k])
	}
	s.kustomize.Versions = nil
	for _, name := range slices.Sorted(maps.Keys(versions)) {
		s.kustomize.Versions = append(s.kustomize.Versions, versions[name])
	}
}

// templateFuncs are the sprig functions available to templates, without the functions
// upstream removes (env, expandenv, getHostByName).
func templateFuncs() template.FuncMap {
	f := sprig.TxtFuncMap()
	delete(f, "env")
	delete(f, "expandenv")
	delete(f, "getHostByName")
	return f
}

func checkTemplate(v string) error {
	_, err := template.New("t").Funcs(templateFuncs()).Parse(v)
	return err
}
