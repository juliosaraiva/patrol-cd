package project

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Nil inputs never panic: callers pass objects read from caches that may be missing.
func TestNilInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assert.NotPanics(t, func() {
		_, _, _ = GetRoleByName(nil, "r")
		_ = ProjectPoliciesString(nil)
		_ = RBACRoles(nil)
		NormalizePolicies(nil)
		_ = NormalizeJWTTokens(nil)
		_ = AddJWTToken(nil, "r", v1alpha1.JWTToken{})
		_, _ = RemoveJWTToken(nil, "r", 1, "")
		_ = ValidateJWTTokenID(nil, "r", "x")
		_, _, _ = GetJWTToken(nil, "r", 1, "")
		_, _ = GlobalProjects(ctx, nil, []GlobalProjectSetting{{ProjectName: "x"}}, nil)
		_ = MergeVirtualProject(nil, &v1alpha1.AppProject{})
		_, _ = VirtualProject(ctx, nil, nil, nil)
		_, _, _ = EffectiveProject(ctx, nil, Lookups{})
		_, _, _ = ResolveAppProject(ctx, nil, "ns", Lookups{})
		_ = CheckAppNamespace(nil, nil, "ns")
		_, _ = EffectiveSourceIntegrity(nil)
		_ = RequiredSignatureKeys(nil)
		_ = NormalizeSyncWindowTimeZones(nil)
		_ = AddSyncWindow(nil, v1alpha1.SyncWindow{})
		_ = DeleteSyncWindow(nil, 0)
		_ = UpdateSyncWindow(nil, SyncWindowUpdate{Duration: "1h"})
		_ = IsSourcePermitted(nil, "x")
		_, _ = IsDestinationPermitted(ctx, nil, Destination{Server: "s"}, nil)
		_ = IsGroupKindPermitted(nil, gk("", "Pod"), true)
		_, _ = IsLiveResourcePermitted(ctx, nil, nil, nil, nil)
		_ = ValidateSyncPermissions(ctx, nil, liveObj("", "v1", "Pod", "ns", "p"), ResourceScope{Known: true, Namespaced: true}, nil, nil)
		_, _ = DeriveServiceAccount(nil, nil, "s")
		_, _ = SelectServiceAccount(nil, nil, "s", ImpersonationSettings{Enabled: true, Enforced: true})
		_ = MatchSyncWindows(nil, nil)
		_, _ = ActiveWindows(nil, nil, time.Now())
		_, _ = CanSync(nil, nil, true, nil, time.Now())
		_, _ = GetApplicationSyncWindowsState(nil, nil, time.Now())
		_ = ValidateApplication(ctx, nil, nil, nil, ValidateOptions{})
		_ = ValidateProject(nil)
		_ = OrphanedResourcesWarn(nil)
		_ = IsKnownOrphanedResourceExclusion(nil, v1alpha1.ResourceKey{})
		_ = IsOrphanCandidate(nil, v1alpha1.ResourceKey{Namespace: "x"})
		_, _ = GitVerificationFor(nil, v1alpha1.ApplicationSource{}, true)
	})
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p"}, Destination: v1alpha1.ApplicationDestination{Server: "https://s"}}}
	assert.NotPanics(t, func() {
		_ = ValidateApplication(ctx, app, &v1alpha1.AppProject{}, nil, ValidateOptions{})
	})
}

// Untrusted schedules, durations, time zones and patterns never panic.
func TestPropertyUntrustedInputsNeverPanic(t *testing.T) {
	t.Parallel()
	text := rapid.OneOf(
		rapid.String(),
		rapid.SampledFrom([]string{"TZ=", "TZ=UTC", "CRON_TZ=", "TZ= ", "TZ=x y", "@daily", "@every", "* * * * *", "0 0 30 2 *", "*/0 * * * *", "60 * * * *", "{", "[", "!", "!*", "**", "/", "//", "/(/"}),
		rapid.Custom(func(t *rapid.T) string {
			return rapid.SampledFrom([]string{"TZ=", "CRON_TZ=", ""}).Draw(t, "prefix") + rapid.StringMatching(`[ *0-9/,\-a-zA-Z@=]{0,24}`).Draw(t, "rest")
		}),
	)
	rapid.Check(t, func(t *rapid.T) {
		w := v1alpha1.SyncWindow{
			Kind:         rapid.SampledFrom([]string{"allow", "deny", ""}).Draw(t, "kind"),
			Schedule:     text.Draw(t, "schedule"),
			Duration:     text.Draw(t, "duration"),
			TimeZone:     text.Draw(t, "tz"),
			Applications: []string{text.Draw(t, "app")},
			Clusters:     []string{text.Draw(t, "cluster")},
			Namespaces:   []string{text.Draw(t, "ns")},
		}
		now := time.Unix(rapid.Int64Range(0, 4102444800).Draw(t, "unix"), 0)
		app := &v1alpha1.Application{}
		app.Name = text.Draw(t, "name")
		app.Namespace = text.Draw(t, "appns")
		app.Spec.Destination = v1alpha1.ApplicationDestination{Server: text.Draw(t, "server"), Name: text.Draw(t, "cname"), Namespace: text.Draw(t, "dns")}
		proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{
			SyncWindows:      v1alpha1.SyncWindows{w},
			SourceRepos:      []string{text.Draw(t, "repo")},
			SourceNamespaces: []string{text.Draw(t, "srcns")},
			Destinations:     []v1alpha1.ApplicationDestination{{Server: text.Draw(t, "ds"), Name: text.Draw(t, "dn"), Namespace: text.Draw(t, "dns2")}},
			Roles:            []v1alpha1.ProjectRole{{Name: text.Draw(t, "role"), Policies: []string{text.Draw(t, "policy")}, Groups: []string{text.Draw(t, "group")}}},
			DestinationServiceAccounts: []v1alpha1.ApplicationDestinationServiceAccount{{
				Server: text.Draw(t, "sas"), Namespace: text.Draw(t, "sans"), DefaultServiceAccount: text.Draw(t, "sa"),
			}},
		}}
		_ = ValidateProject(proj)
		_, _ = CanSync(proj, app, rapid.Bool().Draw(t, "manual"), &now, now)
		_, _ = GetApplicationSyncWindowsState(proj, app, now)
		_ = IsSourcePermitted(proj, text.Draw(t, "url"))
		_ = IsAppNamespacePermitted(proj, app.Namespace, "patrol")
		_, _ = IsDestinationPermitted(context.Background(), proj, Destination{Server: app.Spec.Destination.Server, Name: app.Spec.Destination.Name, Namespace: app.Spec.Destination.Namespace}, nil)
		_, _ = DeriveServiceAccount(proj, app, app.Spec.Destination.Server)
		_ = NormalizeGitURL(text.Draw(t, "url2"))
	})
}
