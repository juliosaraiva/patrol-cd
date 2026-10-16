package settings

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// Validation groups of "patrol admin settings validate --group" (upstream groups plus the
// Patrol sources upstream validates elsewhere).
const (
	GroupGeneral           = "general"
	GroupKustomize         = "kustomize"
	GroupAccounts          = "accounts"
	GroupResourceOverrides = "resource-overrides"
	GroupRBAC              = "rbac"
	GroupCmdParams         = "cmd-params"
	GroupCertificates      = "certificates"
	GroupGPGKeys           = "gpg-keys"
	GroupNotifications     = "notifications"
)

// ValidationGroups returns every validation group, sorted.
func ValidationGroups() []string {
	g := []string{GroupGeneral, GroupKustomize, GroupAccounts, GroupResourceOverrides, GroupRBAC, GroupCmdParams, GroupCertificates, GroupGPGKeys, GroupNotifications}
	slices.Sort(g)
	return g
}

// GroupResult is the outcome of one validation group.
type GroupResult struct {
	Group string
	// OK is false when the group has an error-severity issue.
	OK bool
	// Summary is a one-line description in upstream style ("SSO is not configured",
	// "3 accounts", ...).
	Summary string
	// Issues are the group's issues.
	Issues []Issue
}

// issueGroup assigns an issue to a validation group.
func issueGroup(is Issue) string {
	switch is.Source {
	case SourceRBACConfigMap:
		return GroupRBAC
	case SourceCmdParamsConfigMap:
		return GroupCmdParams
	case SourceTLSCertsConfigMap, SourceSSHKnownHostsConfigMap:
		return GroupCertificates
	case SourceGPGKeysConfigMap:
		return GroupGPGKeys
	case SourceNotificationsConfigMap, SourceNotificationsSecret:
		return GroupNotifications
	}
	k := is.Key
	switch {
	case strings.HasPrefix(k, "kustomize."):
		return GroupKustomize
	case strings.HasPrefix(k, KeyAccountsPrefix) || strings.HasPrefix(k, "admin."):
		return GroupAccounts
	case k == KeyResourceCustomizations || strings.HasPrefix(k, KeyResourceCustomizations+".") || k == KeyResourceCompareOptions:
		return GroupResourceOverrides
	}
	return GroupGeneral
}

// ValidateGroups validates the requested groups (all when none is given) of a snapshot. An
// unknown group name yields an error.
func ValidateGroups(s *Settings, groups ...string) ([]GroupResult, error) {
	if len(groups) == 0 {
		groups = ValidationGroups()
	}
	byGroup := map[string][]Issue{}
	for _, is := range s.issues {
		g := issueGroup(is)
		byGroup[g] = append(byGroup[g], is)
	}
	out := make([]GroupResult, 0, len(groups))
	for _, g := range groups {
		if !slices.Contains(ValidationGroups(), g) {
			return nil, fmt.Errorf("unknown validation group %q; valid groups: %s", g, strings.Join(ValidationGroups(), ", "))
		}
		r := GroupResult{Group: g, OK: !HasErrors(byGroup[g]), Issues: byGroup[g], Summary: s.groupSummary(g)}
		out = append(out, r)
	}
	return out, nil
}

func (s *Settings) groupSummary(group string) string {
	switch group {
	case GroupGeneral:
		provider := ""
		switch {
		case s.dexConfig != "":
			provider = "Dex"
		case s.oidcConfig != nil:
			provider = "OIDC"
		}
		if provider == "" {
			return "SSO is not configured"
		}
		if s.url == "" {
			return provider + " is configured ('url' field is missing)"
		}
		return provider + " is configured"
	case GroupKustomize:
		summary := "default options"
		if s.kustomize.BuildOptions != "" {
			summary = s.kustomize.BuildOptions
		}
		if n := len(s.kustomize.Versions); n > 0 {
			summary = fmt.Sprintf("%s (%d versions)", summary, n)
		}
		return summary
	case GroupAccounts:
		return fmt.Sprintf("%d accounts", len(s.accounts))
	case GroupResourceOverrides:
		// Upstream counts the /status override of ignoreResourceStatusField as an override.
		keys := maps.Clone(s.resourceOverrides)
		switch s.compareOptions.IgnoreResourceStatusField {
		case diff.IgnoreStatusAll:
			keys["*/*"] = luaOverride{}
		case diff.IgnoreStatusCRD:
			keys["apiextensions.k8s.io/CustomResourceDefinition"] = luaOverride{}
		}
		return fmt.Sprintf("%d resource overrides", len(keys))
	case GroupRBAC:
		if !s.rbac.Present {
			return "no RBAC ConfigMap (built-in policy only)"
		}
		return fmt.Sprintf("%d policy documents, default role %q, match mode %s", len(s.rbac.Sources), s.rbac.DefaultRole, s.rbac.MatchMode)
	case GroupCmdParams:
		return fmt.Sprintf("%d parameters set", len(s.cmdParams))
	case GroupCertificates:
		return fmt.Sprintf("%d TLS certificate hosts, %d SSH known hosts", len(s.tlsCerts), len(s.sshKnownHosts))
	case GroupGPGKeys:
		return fmt.Sprintf("%d GnuPG keys", len(s.gpgKeys))
	case GroupNotifications:
		if s.notifications.ConfigMap == nil {
			return "notifications are not configured"
		}
		return fmt.Sprintf("%d notification keys", len(s.notifications.ConfigMap))
	}
	return ""
}
