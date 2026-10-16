package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// logClient serves canned log streams and records the queries.
type logClient struct {
	*fakeClient
	streams [][]logItem
	queries []apiclient.ApplicationLogsQuery
}

type logItem struct {
	entry *apiclient.LogEntry
	err   error
}

type sliceStream struct{ items []logItem }

func (s *sliceStream) Next() (*apiclient.LogEntry, error) {
	if len(s.items) == 0 {
		return nil, io.EOF
	}
	it := s.items[0]
	s.items = s.items[1:]
	return it.entry, it.err
}

func (s *sliceStream) Close() error { return nil }

func (l *logClient) StreamApplicationLogs(_ context.Context, q *apiclient.ApplicationLogsQuery) (LogStream, error) {
	l.queries = append(l.queries, *q)
	if len(l.streams) == 0 {
		return &sliceStream{}, nil
	}
	s := l.streams[0]
	l.streams = l.streams[1:]
	return &sliceStream{items: s}, nil
}

func line(s string) logItem { return logItem{entry: &apiclient.LogEntry{Content: s}} }

func TestLogsCommand(t *testing.T) {
	t.Parallel()
	unavailable := &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Problem: apiclient.Problem{Status: http.StatusServiceUnavailable, Code: apiclient.CodeUnavailable}}
	tests := []struct {
		name    string
		args    []string
		streams [][]logItem
		check   func(t *testing.T, out string, qs []apiclient.ApplicationLogsQuery)
		code    int
	}{
		{
			name:    "flags map to the query; escape sequences are neutralized",
			args:    []string{"app", "logs", "team/web", "--kind", "Deployment", "--group", "apps", "--name", "web", "-c", "app", "--tail", "0", "--filter", "err", "-m", "-p"},
			streams: [][]logItem{{line("one"), line("two \x1b[31mred"), {entry: &apiclient.LogEntry{Last: true}}, line("after last")}},
			check: func(t *testing.T, out string, qs []apiclient.ApplicationLogsQuery) {
				require.Len(t, qs, 1)
				q := qs[0]
				assert.Equal(t, "web", q.Name)
				assert.Equal(t, "team", q.AppNamespace)
				assert.Equal(t, "Deployment", q.Kind)
				assert.Equal(t, "apps", q.Group)
				assert.Equal(t, "web", q.ResourceName)
				assert.Equal(t, "app", q.Container)
				assert.Equal(t, "err", q.Filter)
				require.NotNil(t, q.TailLines, "--tail 0 was set explicitly")
				assert.EqualValues(t, 0, *q.TailLines)
				assert.Nil(t, q.SinceSeconds)
				assert.Nil(t, q.Follow)
				require.NotNil(t, q.MatchCase)
				require.NotNil(t, q.Previous)
				assert.NotContains(t, out, "\x1b")
				assert.True(t, strings.HasPrefix(out, "one\ntwo "), out)
				assert.NotContains(t, out, "after last")
			},
		},
		{
			name:    "follow reconnects after an unavailable stream from the last second",
			args:    []string{"app", "logs", "web", "-f", "--tail", "5"},
			streams: [][]logItem{{line("a"), {err: unavailable}}, {line("b")}},
			check: func(t *testing.T, out string, qs []apiclient.ApplicationLogsQuery) {
				require.Len(t, qs, 2)
				require.NotNil(t, qs[0].TailLines)
				assert.Nil(t, qs[1].TailLines)
				require.NotNil(t, qs[1].SinceSeconds)
				assert.EqualValues(t, 1, *qs[1].SinceSeconds)
				assert.Equal(t, "a\nb\n", out)
			},
		},
		{
			name:    "without follow a stream error is fatal",
			args:    []string{"app", "logs", "web"},
			streams: [][]logItem{{line("a"), {err: unavailable}}},
			code:    1,
		},
		{name: "negative tail", args: []string{"app", "logs", "web", "--tail", "-1"}, code: 1},
		{name: "missing name", args: []string{"app", "logs"}, code: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := &logClient{fakeClient: newFakeClient(), streams: tc.streams}
			out, _, err := runCmd(t, testEnvWith(cl), "", tc.args...)
			if tc.code != 0 {
				requireExitCode(t, err, tc.code)
				return
			}
			require.NoError(t, err)
			tc.check(t, out, cl.queries)
		})
	}
}

func TestCheckResourceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		w              watchOpts
		health, sync   string
		hydrated, want bool
	}{
		{"nothing requested", watchOpts{}, "Missing", "OutOfSync", false, true},
		{"sync", watchOpts{sync: true}, "Missing", "Synced", false, true},
		{"sync not reached", watchOpts{sync: true}, "Healthy", "OutOfSync", false, false},
		{"health", watchOpts{health: true}, "Healthy", "OutOfSync", false, true},
		{"health or suspended", watchOpts{health: true, suspended: true}, "Suspended", "", false, true},
		{"degraded", watchOpts{degraded: true}, "Degraded", "", false, true},
		{"degraded not reached", watchOpts{degraded: true}, "Healthy", "", false, false},
		{"hydrated", watchOpts{hydrated: true}, "", "", true, true},
		{"not hydrated", watchOpts{hydrated: true}, "", "", false, false},
		{"delete never matches a state", watchOpts{delete: true}, "Healthy", "Synced", true, false},
		{"sync and health", watchOpts{sync: true, health: true}, "Healthy", "Synced", false, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, checkResourceStatus(tc.w, tc.health, tc.sync, tc.hydrated), tc.name)
	}
	assert.Equal(t, watchOpts{sync: true, health: true, operation: true}, watchOpts{}.withDefaults())
	assert.Equal(t, watchOpts{suspended: true}, watchOpts{suspended: true}.withDefaults())
}

func TestNeedsReconcile(t *testing.T) {
	t.Parallel()
	finished := metav1.NewTime(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC))
	before := metav1.NewTime(finished.Add(-time.Minute))
	op := &v1alpha1.Operation{Spec: v1alpha1.OperationSpec{Sync: &v1alpha1.SyncOperation{}},
		Status: v1alpha1.OperationStatus{Phase: v1alpha1.OperationSucceeded, FinishedAt: &finished}}
	app := &v1alpha1.Application{}
	assert.True(t, needsReconcile(app, op), "never reconciled")
	app.Status.ReconciledAt = &before
	assert.True(t, needsReconcile(app, op), "reconciled before the operation finished")
	app.Status.ReconciledAt = &finished
	assert.False(t, needsReconcile(app, op))
	app.Status.ReconciledAt = &before
	dry := op.DeepCopy()
	dry.Spec.Sync.DryRun = true
	assert.False(t, needsReconcile(app, dry), "dry runs change nothing")
	running := op.DeepCopy()
	running.Status.Phase = v1alpha1.OperationRunning
	assert.False(t, needsReconcile(app, running))
}

func TestParseActionParams(t *testing.T) {
	t.Parallel()
	got, err := parseActionParams([]string{"replicas=3", "expr=a=b"})
	require.NoError(t, err)
	assert.Equal(t, []apiclient.ResourceActionParameter{{Name: "expr", Value: "a=b"}, {Name: "replicas", Value: "3"}}, got)
	for _, bad := range [][]string{{"novalue"}, {"=x"}, {"a=1", "a=2"}} {
		_, err := parseActionParams(bad)
		assert.Error(t, err, bad)
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://redacted@example.com/app.yaml", redactURL("https://user:secret@example.com/app.yaml"))
	assert.Equal(t, "https://example.com/app.yaml", redactURL("https://example.com/app.yaml"))
	assert.NotContains(t, redactURL("https://tok@example.com/x"), "tok")
}

func TestFilterFieldsFromObject(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg", "labels": map[string]any{"a": "1"}},
		"data":     map[string]any{"k": "v", "other": "x"},
	}}
	got := filterFieldsFromObject(obj, []string{"data.k", "metadata.labels", "missing.field"})
	assert.Equal(t, map[string]any{
		"metadata": map[string]any{"name": "cfg", "labels": map[string]any{"a": "1"}},
		"data":     map[string]any{"k": "v"},
	}, got.Object)
}

func TestPrintHistoryTable(t *testing.T) {
	t.Parallel()
	at := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	history := v1alpha1.RevisionHistories{
		{ID: 0, Revision: "0123456789abcdef0123456789abcdef01234567", DeployedAt: at, Source: &v1alpha1.ApplicationSource{RepoURL: "https://git/a", TargetRevision: "main"}},
		{ID: 1, Revisions: []string{"aaaaaaaaaa", "bbbbbbbbbb"}, DeployedAt: at, Sources: v1alpha1.ApplicationSources{
			{RepoURL: "https://git/a", TargetRevision: "v1"}, {RepoURL: "https://git/b", TargetRevision: "v2"},
		}},
	}
	var buf bytes.Buffer
	printHistoryTable(&buf, history)
	out := buf.String()
	assert.Contains(t, out, "SOURCE  https://git/a")
	assert.Contains(t, out, "SOURCE  https://git/b")
	assert.Contains(t, out, "main (0123456)")
	assert.Contains(t, out, "v1 (aaaaaaa)")
	assert.Contains(t, out, "v2 (bbbbbbb)")
	assert.Less(t, strings.Index(out, "https://git/a"), strings.Index(out, "https://git/b"), "repositories in first-seen order")

	buf.Reset()
	printHistoryIDs(&buf, history)
	assert.Equal(t, "0\n1\n", buf.String())
}

func TestAppChanged(t *testing.T) {
	t.Parallel()
	a := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Project: "default", Source: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p"}}}
	b := a.DeepCopy()
	assert.False(t, appChanged(a, b, true))
	assert.False(t, appChanged(a, b, false))
	b.Labels = map[string]string{}
	assert.False(t, appChanged(a, b, true), "empty and nil labels are equal")
	b.Spec.Source.Path = "q"
	assert.True(t, appChanged(a, b, true))
	assert.False(t, appChanged(a, b, false), "without upsert nothing changes")

	s1 := &v1alpha1.ApplicationSet{}
	s2 := s1.DeepCopy()
	s2.Finalizers = []string{v1alpha1.FinalizerApplicationSet}
	assert.False(t, appSetChanged(s1, s2, true), "the controller's finalizer is not a change")
	s2.Labels = map[string]string{"x": "y"}
	assert.True(t, appSetChanged(s1, s2, true))
}

// TestSetUnsetSourceHydratorApp runs app set / app unset on an Application that uses a
// source hydrator: tool options and -p reach spec.sourceHydrator.drySource, and flags that
// cannot apply fail without sending a spec (no false success).
func TestSetUnsetSourceHydratorApp(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "hyd", Namespace: "patrol"}, Spec: v1alpha1.ApplicationSpec{
		SourceHydrator: &v1alpha1.SourceHydrator{
			DrySource:  v1alpha1.DrySource{RepoURL: "https://g", TargetRevision: "HEAD", Path: "app"},
			SyncSource: v1alpha1.SyncSource{TargetBranch: "env/dev", Path: "out"},
		}}}

	// The reviewer's case: a location flag with a tool option fails, nothing is sent.
	fc := newFakeClient(app)
	_, _, err := runCmd(t, testEnvWith(fc), "", "app", "set", "hyd", "--path", "new-path", "--helm-set", "a=b")
	requireExitCode(t, err, 20)
	assert.Contains(t, err.Error(), "--path cannot be used with an application that uses a source hydrator")
	assert.Empty(t, fc.specs)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "set", "hyd", "--source-position", "1", "-p", "a=b")
	requireExitCode(t, err, 20)
	assert.Empty(t, fc.specs)

	// Tool options and parameter overrides are stored in the dry source.
	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "set", "hyd", "--helm-set", "a=b", "-p", "image.tag=v2", "--helm-version", "v3")
	require.NoError(t, err)
	require.Len(t, fc.specs, 1)
	got := fc.specs[0].Spec
	assert.Nil(t, got.Source)
	want := app.Spec.DeepCopy()
	want.SourceHydrator.DrySource.Helm = &v1alpha1.ApplicationSourceHelm{Version: "v3",
		Parameters: []v1alpha1.HelmParameter{{Name: "a", Value: "b"}, {Name: "image.tag", Value: "v2"}}}
	assert.Equal(t, want, got)

	// unset edits the dry source too.
	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "unset", "hyd", "-p", "a")
	require.NoError(t, err)
	require.Len(t, fc.specs, 2)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v2"}}, fc.specs[1].Spec.SourceHydrator.DrySource.Helm.Parameters)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "unset", "hyd", "--source-position", "1", "-p", "image.tag")
	requireExitCode(t, err, 20)
	assert.Len(t, fc.specs, 2)

	// A tool block emptied by unset is removed from the dry source (not left as {}).
	kust := app.DeepCopy()
	kust.Spec.SourceHydrator.DrySource.Kustomize = &v1alpha1.ApplicationSourceKustomize{Version: "v5"}
	fc = newFakeClient(kust)
	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "unset", "hyd", "--kustomize-version")
	require.NoError(t, err)
	require.Len(t, fc.specs, 1)
	assert.Equal(t, app.Spec, *fc.specs[0].Spec)
}
