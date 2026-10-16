package core

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// pluginsTimeout bounds the repo-server plugin listing.
const pluginsTimeout = 30 * time.Second

// getSettings serves GET /api/v1/settings (upstream SettingsService.Get): a public
// subset for every caller, plus the UI banner, controller namespace and resource
// overrides for logged-in callers (or with authentication disabled), plus the password
// pattern for logged-in callers only. Secrets (OIDC client secret, Dex connector
// configuration) are never returned.
func (h *Handlers) getSettings(ctx context.Context, _ *apiclient.Empty) (*apiclient.Settings, error) {
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	out := publicSettings(s, h.d.Config)
	id := rest.IdentityFrom(ctx)
	loggedIn := id.LoggedIn()
	if loggedIn || h.d.Config.DisableAuth || (id != nil && id.AuthDisabled) {
		ui := s.UI()
		out.UIBannerContent = ui.BannerContent
		out.UIBannerURL = ui.BannerURL
		out.UIBannerPermanent = ui.BannerPermanent
		out.UIBannerPosition = ui.BannerPosition
		out.ControllerNamespace = s.Namespace()
		if out.ControllerNamespace == "" {
			out.ControllerNamespace = h.d.Config.Namespace
		}
		out.ResourceOverrides = resourceOverrides(ctx, h.log(), s.ResourceOverrides())
	}
	if loggedIn {
		out.PasswordPattern = s.PasswordPattern()
	}
	return out, nil
}

func publicSettings(s *settings.Settings, cfg Config) *apiclient.Settings {
	badge := s.StatusBadge()
	ga := s.GoogleAnalytics()
	help := s.Help()
	k := s.Kustomize()
	ui := s.UI()
	out := &apiclient.Settings{
		URL:                       s.URL(),
		AdditionalURLs:            s.AdditionalURLs(),
		AppLabelKey:               s.InstanceLabelKey(),
		StatusBadgeEnabled:        badge.Enabled,
		StatusBadgeRootURL:        badge.RootURL,
		UserLoginsDisabled:        s.UserLoginsDisabled(),
		AnonymousEnabled:          s.AnonymousUserEnabled(),
		UICSSURL:                  ui.CSSURL,
		TrackingMethod:            string(s.TrackingMethod()),
		ExecEnabled:               s.Exec().Enabled,
		AppsInAnyNamespaceEnabled: cfg.AppsInAnyNamespaceEnabled,
		ImpersonationEnabled:      s.Impersonation().Enabled,
		InstallationID:            s.InstallationID(),
		HydratorEnabled:           cfg.HydratorEnabled,
		SyncWithReplaceAllowed:    cfg.SyncWithReplaceAllowed,
	}
	if ga.TrackingID != "" || ga.AnonymizeUsers {
		out.GoogleAnalytics = &apiclient.GoogleAnalyticsConfig{TrackingID: ga.TrackingID, AnonymizeUsers: ga.AnonymizeUsers}
	}
	if k.BuildOptions != "" {
		out.KustomizeOptions = &apiclient.KustomizeOptions{BuildOptions: k.BuildOptions}
	}
	for _, v := range k.Versions {
		out.KustomizeVersions = append(out.KustomizeVersions, v.Name)
	}
	if help.ChatURL != "" || len(help.BinaryURLs) > 0 {
		out.Help = &apiclient.Help{ChatURL: help.ChatURL, ChatText: help.ChatText, BinaryURLs: maps.Clone(help.BinaryURLs)}
	}
	if s.IsDexConfigured() {
		dc := &apiclient.DexConfig{}
		for _, c := range s.DexConnectors() {
			dc.Connectors = append(dc.Connectors, apiclient.DexConnector{Name: c.Name, Type: c.Type})
		}
		out.DexConfig = dc
	}
	if oc := s.OIDCConfig(); oc != nil {
		o := &apiclient.OIDCConfig{
			Name:                     oc.Name,
			Issuer:                   oc.Issuer,
			ClientID:                 oc.ClientID,
			CLIClientID:              oc.CLIClientID,
			Scopes:                   slices.Clone(oc.RequestedScopes),
			EnablePKCEAuthentication: oc.EnablePKCEAuthentication,
		}
		if len(oc.RequestedIDTokenClaims) > 0 {
			o.IDTokenClaims = make(map[string]*apiclient.OIDCClaim, len(oc.RequestedIDTokenClaims))
			for name, c := range oc.RequestedIDTokenClaims {
				if c == nil {
					o.IDTokenClaims[name] = nil
					continue
				}
				o.IDTokenClaims[name] = &apiclient.OIDCClaim{Essential: c.Essential, Value: c.Value, Values: slices.Clone(c.Values)}
			}
		}
		out.OIDCConfig = o
	}
	return out
}

// resourceOverrides converts the parsed resource customizations; entries whose ignore
// rules cannot be decoded keep their other fields (the settings parser validated them,
// so this only happens on a programming error and is logged).
func resourceOverrides(ctx context.Context, lg *slog.Logger, in map[string]lua.ResourceOverride) map[string]apiclient.ResourceOverride {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]apiclient.ResourceOverride, len(in))
	for key, o := range in {
		r := apiclient.ResourceOverride{HealthLua: o.HealthLua, UseOpenLibs: o.UseOpenLibs, Actions: o.Actions}
		if o.IgnoreDifferences != "" {
			if d, err := o.GetIgnoreDifferences(); err == nil {
				r.IgnoreDifferences = ignoreDiff(d)
			} else {
				lg.LogAttrs(ctx, slog.LevelWarn, "invalid ignoreDifferences in resource customization",
					slog.String("key", key), slog.Any("error", err))
			}
		}
		if o.IgnoreResourceUpdates != "" {
			if d, err := o.GetIgnoreResourceUpdates(); err == nil {
				r.IgnoreResourceUpdates = ignoreDiff(d)
			} else {
				lg.LogAttrs(ctx, slog.LevelWarn, "invalid ignoreResourceUpdates in resource customization",
					slog.String("key", key), slog.Any("error", err))
			}
		}
		for _, f := range o.KnownTypeFields {
			r.KnownTypeFields = append(r.KnownTypeFields, apiclient.KnownTypeField{Field: f.Field, Type: f.Type})
		}
		out[key] = r
	}
	return out
}

func ignoreDiff(d lua.IgnoreDiff) *apiclient.OverrideIgnoreDiff {
	return &apiclient.OverrideIgnoreDiff{
		JSONPointers:          slices.Clone(d.JSONPointers),
		JQPathExpressions:     slices.Clone(d.JQPathExpressions),
		ManagedFieldsManagers: slices.Clone(d.ManagedFieldsManagers),
	}
}

// getPluginSettings serves GET /api/v1/settings/plugins: the config management plugins
// announced by the repo-server's sidecars.
func (h *Handlers) getPluginSettings(ctx context.Context, _ *apiclient.Empty) (*apiclient.PluginSettings, error) {
	if h.d.RepoServer == nil {
		return nil, rest.Unavailable("the repo-server is not configured").WithCause(errNotConfigured)
	}
	ctx, cancel := withTimeout(ctx, pluginsTimeout)
	defer cancel()
	list, err := h.d.RepoServer.ListPlugins(ctx)
	if err != nil {
		return nil, rest.Unavailable("failed to list sidecar plugins from repo-server").WithCause(err)
	}
	out := &apiclient.PluginSettings{}
	if list != nil {
		for _, p := range list.Items {
			out.Plugins = append(out.Plugins, apiclient.Plugin{Name: p.Name})
		}
	}
	slices.SortFunc(out.Plugins, func(a, b apiclient.Plugin) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out, nil
}
