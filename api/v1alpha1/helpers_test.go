package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

func TestApplicationSpecGetSources(t *testing.T) {
	t.Parallel()
	single := ApplicationSource{RepoURL: "https://example.com/a.git", Path: "a"}
	multi := ApplicationSources{
		{RepoURL: "https://example.com/a.git", Path: "a"},
		{RepoURL: "https://example.com/b.git", Ref: "values"},
	}
	hydrator := &SourceHydrator{
		DrySource:  DrySource{RepoURL: "https://example.com/dry.git", TargetRevision: "main", Path: "env/prod"},
		SyncSource: SyncSource{TargetBranch: "env/prod-next", Path: "out"},
	}

	tests := []struct {
		name      string
		spec      ApplicationSpec
		want      ApplicationSources
		wantMulti bool
	}{
		{name: "none", spec: ApplicationSpec{}, want: ApplicationSources{}},
		{name: "single", spec: ApplicationSpec{Source: &single}, want: ApplicationSources{single}},
		{name: "multi", spec: ApplicationSpec{Sources: multi}, want: multi, wantMulti: true},
		{name: "empty sources falls back to source", spec: ApplicationSpec{Source: &single, Sources: ApplicationSources{}}, want: ApplicationSources{single}},
		{name: "sources win over source", spec: ApplicationSpec{Source: &single, Sources: multi}, want: multi, wantMulti: true},
		{
			name: "hydrator syncs from sync source with dry repo",
			spec: ApplicationSpec{SourceHydrator: hydrator, Sources: multi},
			want: ApplicationSources{{RepoURL: "https://example.com/dry.git", Path: "out", TargetRevision: "env/prod-next"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.spec.GetSources())
			assert.Equal(t, tt.wantMulti, tt.spec.HasMultipleSources())
		})
	}
}

func TestSourceHydratorSources(t *testing.T) {
	t.Parallel()
	h := SourceHydrator{
		DrySource:  DrySource{RepoURL: "https://example.com/dry.git", TargetRevision: "main", Path: "src", Helm: &ApplicationSourceHelm{ReleaseName: "r"}},
		SyncSource: SyncSource{TargetBranch: "hydrated", Path: "out", RepoURL: "https://example.com/hydrated.git"},
	}
	assert.Equal(t, ApplicationSource{RepoURL: "https://example.com/hydrated.git", Path: "out", TargetRevision: "hydrated"}, h.GetSyncSource())
	assert.Equal(t, ApplicationSource{RepoURL: "https://example.com/dry.git", Path: "src", TargetRevision: "main", Helm: &ApplicationSourceHelm{ReleaseName: "r"}}, h.GetDrySource())
}

func TestIsAutomatedSyncEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy *SyncPolicy
		want   bool
	}{
		{name: "nil policy", policy: nil, want: false},
		{name: "no automated", policy: &SyncPolicy{}, want: false},
		{name: "automated empty", policy: &SyncPolicy{Automated: &SyncPolicyAutomated{}}, want: true},
		{name: "automated enabled", policy: &SyncPolicy{Automated: &SyncPolicyAutomated{Enabled: ptr.To(true)}}, want: true},
		{name: "automated disabled", policy: &SyncPolicy{Automated: &SyncPolicyAutomated{Enabled: ptr.To(false)}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.policy.IsAutomatedSyncEnabled())
			spec := ApplicationSpec{SyncPolicy: tt.policy}
			assert.Equal(t, tt.want, spec.IsAutomatedSyncEnabled())
		})
	}
}

func TestSyncPolicyAutomatedGetters(t *testing.T) {
	t.Parallel()
	var nilAutomated *SyncPolicyAutomated
	assert.False(t, nilAutomated.GetPrune())
	assert.False(t, nilAutomated.GetSelfHeal())
	assert.False(t, nilAutomated.GetAllowEmpty())

	a := &SyncPolicyAutomated{Prune: ptr.To(true), SelfHeal: ptr.To(false), AllowEmpty: ptr.To(true)}
	assert.True(t, a.GetPrune())
	assert.False(t, a.GetSelfHeal())
	assert.True(t, a.GetAllowEmpty())
}

func TestGetProject(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultAppProjectName, (&ApplicationSpec{}).GetProject())
	assert.Equal(t, "team-a", (&ApplicationSpec{Project: "team-a"}).GetProject())
}

func TestParseSyncOption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in        string
		key, val  string
		wantFound bool
	}{
		{in: "Prune=false", key: "Prune", val: "false", wantFound: true},
		{in: " CreateNamespace = true ", key: "CreateNamespace", val: "true", wantFound: true},
		{in: "PrunePropagationPolicy=foreground", key: "PrunePropagationPolicy", val: "foreground", wantFound: true},
		{in: "Key=a=b", key: "Key", val: "a=b", wantFound: true},
		{in: "Key=", key: "Key", val: "", wantFound: true},
		{in: "Standalone", key: "Standalone", val: "", wantFound: false},
		{in: "", key: "", val: "", wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			k, v, ok := ParseSyncOption(tt.in)
			assert.Equal(t, tt.key, k)
			assert.Equal(t, tt.val, v)
			assert.Equal(t, tt.wantFound, ok)
		})
	}
}

func TestSyncOptions(t *testing.T) {
	t.Parallel()
	opts := SyncOptions{"Validate=false", "CreateNamespace=true", "PruneLast=true", "CreateNamespace=false", "Standalone"}

	assert.True(t, opts.HasOption("Validate=false"))
	assert.False(t, opts.HasOption("Validate=true"))
	assert.True(t, opts.HasOption("Standalone"))

	v, ok := opts.GetOptionValue(SyncOptionKeyCreateNamespace)
	assert.True(t, ok)
	assert.Equal(t, "false", v, "the last occurrence wins")
	_, ok = opts.GetOptionValue("Standalone")
	assert.False(t, ok, "standalone options have no value")
	_, ok = opts.GetOptionValue(SyncOptionKeyServerSideApply)
	assert.False(t, ok)

	assert.True(t, opts.IsOptionTrue(SyncOptionKeyPruneLast))
	assert.False(t, opts.IsOptionTrue(SyncOptionKeyCreateNamespace))
	assert.False(t, opts.IsOptionTrue(SyncOptionKeyValidate))

	added := opts.AddOption("ServerSideApply=true")
	assert.True(t, added.HasOption("ServerSideApply=true"))
	assert.Len(t, added.AddOption("ServerSideApply=true"), len(added), "adding twice is a no-op")

	removed := opts.RemoveOption("PruneLast=true")
	assert.False(t, removed.HasOption("PruneLast=true"))
	assert.True(t, opts.HasOption("PruneLast=true"), "RemoveOption must not mutate the receiver")

	var empty SyncOptions
	assert.False(t, empty.HasOption("x"))
	assert.Empty(t, empty.RemoveOption("x"))
	assert.Equal(t, SyncOptions{"x"}, empty.AddOption("x"))
}

func TestApplicationDestinationEquals(t *testing.T) {
	t.Parallel()
	base := ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "apps"}
	tests := []struct {
		name  string
		other ApplicationDestination
		want  bool
	}{
		{name: "identical", other: base, want: true},
		{name: "namespace differs", other: ApplicationDestination{Server: base.Server, Namespace: "other"}, want: false},
		{name: "server differs", other: ApplicationDestination{Server: "https://other", Namespace: "apps"}, want: false},
		{name: "name instead of server", other: ApplicationDestination{Name: "in-cluster", Namespace: "apps"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, base.Equals(tt.other))
			assert.Equal(t, tt.want, tt.other.Equals(base))
		})
	}
}

func TestResourceKeys(t *testing.T) {
	t.Parallel()
	rs := ResourceStatus{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "ns", Name: "web"}
	key := ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web"}
	assert.Equal(t, key, rs.Key())
	assert.Equal(t, "apps/Deployment/ns/web", key.String())
	assert.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, rs.GroupVersionKind())

	rr := ResourceResult{Group: "", Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cfg"}
	assert.Equal(t, "/ConfigMap/ns/cfg", rr.Key().String())
	assert.Equal(t, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, rr.GroupVersionKind())

	status := ApplicationStatus{Resources: []ResourceStatus{
		{Kind: "Service", Namespace: "ns", Name: "web"},
		rs,
	}}
	found, ok := status.FindResource(key)
	require.True(t, ok)
	assert.Equal(t, "v1", found.Version)
	found.Status = SyncStatusCodeSynced
	assert.Equal(t, SyncStatusCodeSynced, status.Resources[1].Status, "FindResource returns a pointer into the slice")
	_, ok = status.FindResource(ResourceKey{Kind: "Deployment", Namespace: "ns", Name: "web"})
	assert.False(t, ok, "group is part of the key")
}

func TestApplicationStatusGetRevisions(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{}, (&ApplicationStatus{}).GetRevisions())
	assert.Equal(t, []string{"abc"}, (&ApplicationStatus{Sync: SyncStatus{Revision: "abc"}}).GetRevisions())
	assert.Equal(t, []string{"a", "b"}, (&ApplicationStatus{Sync: SyncStatus{Revision: "x", Revisions: []string{"a", "b"}}}).GetRevisions())
}

func TestOperationPhase(t *testing.T) {
	t.Parallel()
	terminal := map[OperationPhase]bool{
		OperationPending:     false,
		OperationRunning:     false,
		OperationTerminating: false,
		OperationSucceeded:   true,
		OperationFailed:      true,
		OperationError:       true,
		OperationCancelled:   true,
		OperationRejected:    true,
		"":                   false,
	}
	for phase, want := range terminal {
		assert.Equal(t, want, phase.IsTerminal(), "phase %q", phase)
		op := Operation{Status: OperationStatus{Phase: phase}}
		assert.Equal(t, want, op.IsTerminal(), "operation in phase %q", phase)
	}
	assert.True(t, OperationSucceeded.IsSuccessful())
	assert.False(t, OperationFailed.IsSuccessful())

	op := Operation{Spec: OperationSpec{Sync: &SyncOperation{DryRun: true}, Cancel: true}}
	assert.True(t, op.IsDryRun())
	assert.True(t, op.IsCancelRequested())
	assert.False(t, (&Operation{}).IsDryRun())
}

func TestApplicationAnnotationsHelpers(t *testing.T) {
	t.Parallel()
	app := Application{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"}}
	_, ok := app.IsRefreshRequested()
	assert.False(t, ok)
	assert.Equal(t, "apps/web", app.QualifiedName())

	for value, want := range map[string]RefreshType{"hard": RefreshTypeHard, "normal": RefreshTypeNormal, "bogus": RefreshTypeNormal} {
		app.Annotations = map[string]string{AnnotationKeyRefresh: value}
		got, ok := app.IsRefreshRequested()
		assert.True(t, ok)
		assert.Equal(t, want, got, "value %q", value)
	}
}

func TestApplicationsSyncPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		p                     ApplicationsSyncPolicy
		allowUpd, allowDelete bool
	}{
		{p: "", allowUpd: true, allowDelete: true},
		{p: ApplicationsSyncPolicySync, allowUpd: true, allowDelete: true},
		{p: ApplicationsSyncPolicyCreateOnly, allowUpd: false, allowDelete: false},
		{p: ApplicationsSyncPolicyCreateUpdate, allowUpd: true, allowDelete: false},
		{p: ApplicationsSyncPolicyCreateDelete, allowUpd: false, allowDelete: true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.allowUpd, tt.p.AllowUpdate(), "policy %q", tt.p)
		assert.Equal(t, tt.allowDelete, tt.p.AllowDelete(), "policy %q", tt.p)
	}
}

func TestOrphanedResourcesIsWarn(t *testing.T) {
	t.Parallel()
	var nilSettings *OrphanedResourcesMonitorSettings
	assert.False(t, nilSettings.IsWarn())
	assert.False(t, (&OrphanedResourcesMonitorSettings{}).IsWarn())
	assert.True(t, (&OrphanedResourcesMonitorSettings{Warn: ptr.To(true)}).IsWarn())
}

func TestNestedGeneratorDecoding(t *testing.T) {
	t.Parallel()
	g, err := ToNestedMatrixGenerator(nil)
	require.NoError(t, err)
	assert.Nil(t, g)

	raw := `{"generators":[{"list":{"elements":[{"a":"1"}]}},{"clusters":{"selector":{"matchLabels":{"env":"prod"}}}}]}`
	mg, err := ToNestedMatrixGenerator(&apiextensionsv1.JSON{Raw: []byte(raw)})
	require.NoError(t, err)
	require.Len(t, mg.Generators, 2)
	require.NotNil(t, mg.Generators[0].List)
	assert.JSONEq(t, `{"a":"1"}`, string(mg.Generators[0].List.Elements[0].Raw))
	assert.Equal(t, "prod", mg.Generators[1].Clusters.Selector.MatchLabels["env"])

	merge, err := ToNestedMergeGenerator(&apiextensionsv1.JSON{Raw: []byte(`{"mergeKeys":["server"],"generators":[{"git":{"repoURL":"r","revision":"HEAD"}},{"list":{"elements":[]}}]}`)})
	require.NoError(t, err)
	assert.Equal(t, []string{"server"}, merge.MergeKeys)
	assert.Equal(t, "r", merge.Generators[0].Git.RepoURL)

	_, err = ToNestedMergeGenerator(&apiextensionsv1.JSON{Raw: []byte(`{"generators":`)})
	assert.Error(t, err)

	// The CRD schema cannot see inside nested generators, so the decoders enforce the generator
	// counts of MatrixGenerator (exactly 2) and MergeGenerator (at least 2).
	one := `{"generators":[{"list":{"elements":[]}}]}`
	three := `{"generators":[{"list":{"elements":[]}},{"list":{"elements":[]}},{"list":{"elements":[]}}]}`
	_, err = ToNestedMatrixGenerator(&apiextensionsv1.JSON{Raw: []byte(one)})
	assert.ErrorContains(t, err, "want exactly 2")
	_, err = ToNestedMatrixGenerator(&apiextensionsv1.JSON{Raw: []byte(three)})
	assert.ErrorContains(t, err, "want exactly 2")
	_, err = ToNestedMergeGenerator(&apiextensionsv1.JSON{Raw: []byte(one)})
	assert.ErrorContains(t, err, "want at least 2")
	mergeOfThree, err := ToNestedMergeGenerator(&apiextensionsv1.JSON{Raw: []byte(three)})
	require.NoError(t, err)
	assert.Len(t, mergeOfThree.Generators, 3)
}

// Empty generator templates, selectors and plugin inputs must not be serialized: Go clients would
// otherwise add `template: {metadata: {}, spec: {...}}` to every generator on update, which shows
// up as drift against the manifest an ApplicationSet was applied from.
func TestEmptyGeneratorFieldsAreOmitted(t *testing.T) {
	t.Parallel()
	gens := []ApplicationSetGenerator{
		{List: &ListGenerator{}},
		{Clusters: &ClusterGenerator{}},
		{Git: &GitGenerator{RepoURL: "r", Revision: "HEAD"}},
		{SCMProvider: &SCMProviderGenerator{}},
		{ClusterDecisionResource: &DuckTypeGenerator{ConfigMapRef: "cm"}},
		{PullRequest: &PullRequestGenerator{}},
		{Matrix: &MatrixGenerator{}},
		{Merge: &MergeGenerator{}},
		{Plugin: &PluginGenerator{ConfigMapRef: PluginConfigMapRef{Name: "p"}}},
	}
	out, err := json.Marshal(gens)
	require.NoError(t, err)
	assert.JSONEq(t, `[
		{"list":{}},
		{"clusters":{}},
		{"git":{"repoURL":"r","revision":"HEAD"}},
		{"scmProvider":{}},
		{"clusterDecisionResource":{"configMapRef":"cm"}},
		{"pullRequest":{}},
		{"matrix":{"generators":null}},
		{"merge":{"generators":null,"mergeKeys":null}},
		{"plugin":{"configMapRef":{"name":"p"}}}
	]`, string(out))

	// A non-empty template is still serialized.
	withTemplate, err := json.Marshal(ListGenerator{Template: ApplicationSetTemplate{ApplicationSetTemplateMeta: ApplicationSetTemplateMeta{Name: "x"}}})
	require.NoError(t, err)
	assert.Contains(t, string(withTemplate), `"template":{"metadata":{"name":"x"}`)
}

func TestPluginParameterJSONRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		param ApplicationSourcePluginParameter
		json  string
	}{
		{name: "string", param: ApplicationSourcePluginParameter{Name: "s", String: ptr.To("v")}, json: `{"name":"s","string":"v"}`},
		{name: "empty map is present", param: ApplicationSourcePluginParameter{Name: "m", OptionalMap: &OptionalMap{}}, json: `{"name":"m","map":{}}`},
		{name: "empty array is present", param: ApplicationSourcePluginParameter{Name: "a", OptionalArray: &OptionalArray{}}, json: `{"name":"a","array":[]}`},
		{name: "values", param: ApplicationSourcePluginParameter{Name: "x", OptionalMap: &OptionalMap{Map: map[string]string{"k": "v"}}, OptionalArray: &OptionalArray{Array: []string{"1"}}}, json: `{"name":"x","map":{"k":"v"},"array":["1"]}`},
		{name: "absent", param: ApplicationSourcePluginParameter{Name: "n"}, json: `{"name":"n"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := json.Marshal(tt.param)
			require.NoError(t, err)
			assert.JSONEq(t, tt.json, string(out))

			var back ApplicationSourcePluginParameter
			require.NoError(t, json.Unmarshal(out, &back))
			assert.Equal(t, tt.param.OptionalMap != nil, back.OptionalMap != nil, "map presence survives")
			assert.Equal(t, tt.param.OptionalArray != nil, back.OptionalArray != nil, "array presence survives")
			assert.Equal(t, tt.param.String, back.String)
		})
	}
}

func TestDeepCopyIsIndependent(t *testing.T) {
	t.Parallel()
	app := &Application{Spec: ApplicationSpec{
		Sources:    ApplicationSources{{RepoURL: "a", Helm: &ApplicationSourceHelm{ValueFiles: []string{"v.yaml"}}}},
		SyncPolicy: &SyncPolicy{SyncOptions: SyncOptions{"Validate=false"}},
	}}
	cp := app.DeepCopy()
	cp.Spec.Sources[0].Helm.ValueFiles[0] = "changed"
	cp.Spec.SyncPolicy.SyncOptions[0] = "changed"
	assert.Equal(t, "v.yaml", app.Spec.Sources[0].Helm.ValueFiles[0])
	assert.Equal(t, "Validate=false", app.Spec.SyncPolicy.SyncOptions[0])
}

func TestKeysUseGroupPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "cd.patrol.example/refresh", AnnotationKeyRefresh)
	assert.Equal(t, "cd.patrol.example/tracking-id", AnnotationKeyTrackingID)
	assert.Equal(t, "notifications.cd.patrol.example/subscribe.", AnnotationKeyNotificationSubscribePrefix)
	assert.Equal(t, "resources-finalizer.cd.patrol.example/background", FinalizerResourcesBackground)
	assert.Equal(t, "post-delete-finalizer.cd.patrol.example/cleanup", FinalizerPostDeleteCleanup)
	assert.Equal(t, "link.cd.patrol.example/", AnnotationKeyLinkPrefix)
	assert.Equal(t, "notifications.cd.patrol.example/subscriptions", AnnotationKeyNotificationSubscriptions)
	assert.Equal(t, "cd.patrol.example/managed-by", AnnotationKeyManagedBy)
	assert.Equal(t, TrackingMethod("annotation+label"), TrackingMethodAnnotationAndLabel)
	assert.Equal(t, "operations.cd.patrol.example", OperationAdmissionPolicyName)
}
