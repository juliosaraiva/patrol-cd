package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
)

// fakeOps is an in-memory OperationGetter.
type fakeOps struct {
	ops   map[string]map[string]any
	err   error
	calls int
}

func (f *fakeOps) GetOperation(_ context.Context, namespace, name string) (map[string]any, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	op, ok := f.ops[namespace+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "cd.patrol.example", Resource: "operations"}, name)
	}
	return op, nil
}

// jsonObj decodes a JSON literal (test fixtures read like the stored objects).
func jsonObj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}

const appWithOperation = `{
  "apiVersion": "cd.patrol.example/v1alpha1", "kind": "Application",
  "metadata": {"name": "guestbook", "namespace": "apps"},
  "spec": {"project": "default"},
  "status": {
    "sync": {"status": "Synced", "revision": "abc"},
    "health": {"status": "Healthy"},
    "history": [{"id": 1, "revision": "abc", "deployedAt": "2026-01-01T00:05:00Z"}],
    "conditions": [
      {"type": "Synced", "status": "True", "reason": "Synced", "message": "ok", "lastTransitionTime": "2026-01-01T00:00:00Z"},
      {"type": "SyncError", "status": "True", "reason": "SyncFailed", "message": "boom", "lastTransitionTime": "2026-01-01T00:01:00Z"},
      {"type": "OrphanedResourceWarning", "status": "False", "reason": "None", "message": "none", "lastTransitionTime": "2026-01-01T00:01:00Z"}
    ],
    "operationState": {
      "operationName": "guestbook-1",
      "phase": "Succeeded",
      "startedAt": "2026-01-01T00:00:00Z",
      "syncResult": {"revision": "abc"},
      "initiatedBy": {"username": "summary-user"}
    }
  }
}`

func operationFixture(app, phase string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"application": app,
			"sync": map[string]any{
				"revision":  "abc",
				"prune":     true,
				"manifests": []any{"apiVersion: v1"},
			},
			"initiatedBy": map[string]any{"username": "alice"},
			"info":        []any{map[string]any{"name": "reason", "value": "release"}},
		},
		"status": map[string]any{
			"phase":      phase,
			"message":    "successfully synced",
			"startedAt":  "2026-01-01T00:00:00Z",
			"finishedAt": "2026-01-01T00:05:00Z",
			"retryCount": int64(1),
			"syncResult": map[string]any{
				"revision": "op-revision",
				"resources": []any{map[string]any{
					"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "apps", "name": "ui",
					"status": "Synced", "message": "created", "syncPhase": "Sync",
				}},
				"managedNamespaceMetadata": map[string]any{"labels": map[string]any{"team": "a"}},
			},
		},
	}
}

func TestTemplateApplicationEnrichesOperationState(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Succeeded")}}
	in := jsonObj(t, appWithOperation)
	before := jsonObj(t, appWithOperation)

	out, err := TemplateApplication(context.Background(), in, ops)
	require.NoError(t, err)
	assert.Equal(t, before, in, "input must not be modified")

	status := out["status"].(map[string]any)
	assert.Equal(t, before["status"].(map[string]any)["history"], status["history"], "history is passed through")
	assert.Equal(t, []any{map[string]any{"type": "SyncError", "message": "boom", "lastTransitionTime": "2026-01-01T00:01:00Z"}}, status["conditions"],
		"only active Argo CD condition types, in the Argo CD shape")

	os := status["operationState"].(map[string]any)
	assert.Equal(t, "Succeeded", os["phase"], "summary fields win")
	assert.Equal(t, "successfully synced", os["message"], "missing fields come from the Operation")
	assert.Equal(t, "2026-01-01T00:05:00Z", os["finishedAt"])
	assert.Equal(t, int64(1), os["retryCount"])
	sr := os["syncResult"].(map[string]any)
	assert.Equal(t, "abc", sr["revision"], "the summary revision wins")
	require.Len(t, sr["resources"], 1)
	assert.Equal(t, "ui", sr["resources"].([]any)[0].(map[string]any)["name"])
	assert.NotNil(t, sr["managedNamespaceMetadata"])

	op := os["operation"].(map[string]any)
	assert.Equal(t, map[string]any{"username": "alice"}, op["initiatedBy"], "the Operation's initiator wins over the summary")
	assert.Equal(t, map[string]any{"revision": "abc", "prune": true}, op["sync"], "manifests are never exposed")
	assert.NotNil(t, op["info"])
	_, hasTop := out["operation"]
	assert.False(t, hasTop, "a terminal Operation is not the app's current operation")

	// The Operation map handed out by the getter is not modified either.
	assert.Equal(t, operationFixture("guestbook", "Succeeded"), ops.ops["apps/guestbook-1"])
}

func TestTemplateApplicationRunningOperationSetsTopLevelOperation(t *testing.T) {
	t.Parallel()
	app := jsonObj(t, appWithOperation)
	os := app["status"].(map[string]any)["operationState"].(map[string]any)
	os["phase"] = "Running"
	delete(os, "syncResult")
	ops := &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Running")}}

	out, err := TemplateApplication(context.Background(), app, ops)
	require.NoError(t, err)
	top, ok := out["operation"].(map[string]any)
	require.True(t, ok, "a running Operation is exposed as .app.operation")
	assert.Equal(t, map[string]any{"username": "alice"}, top["initiatedBy"])
	assert.Equal(t, "op-revision", out["status"].(map[string]any)["operationState"].(map[string]any)["syncResult"].(map[string]any)["revision"])
}

func TestTemplateApplicationEdgeCases(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	tests := []struct {
		name     string
		app      func(*testing.T) map[string]any
		ops      OperationGetter
		wantErr  error
		anyErr   bool
		enriched bool
	}{
		{name: "nil app", app: func(*testing.T) map[string]any { return nil }, anyErr: true},
		{name: "no metadata", app: func(*testing.T) map[string]any { return map[string]any{"spec": map[string]any{}} }, wantErr: errInvalidApplication},
		{name: "no status", app: func(t *testing.T) map[string]any {
			return jsonObj(t, `{"metadata": {"name": "a", "namespace": "apps"}}`)
		}, ops: &fakeOps{}},
		{name: "nil getter", app: func(t *testing.T) map[string]any { return jsonObj(t, appWithOperation) }},
		{name: "operation deleted by retention", app: func(t *testing.T) map[string]any { return jsonObj(t, appWithOperation) }, ops: &fakeOps{}},
		{name: "lookup error is returned", app: func(t *testing.T) map[string]any { return jsonObj(t, appWithOperation) }, ops: &fakeOps{err: errBoom}, wantErr: errBoom},
		{name: "operation of another application", app: func(t *testing.T) map[string]any { return jsonObj(t, appWithOperation) },
			ops: &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("other-app", "Succeeded")}}},
		{name: "re-created operation with another startedAt", app: func(t *testing.T) map[string]any {
			app := jsonObj(t, appWithOperation)
			app["status"].(map[string]any)["operationState"].(map[string]any)["startedAt"] = "2026-02-01T00:00:00Z"
			return app
		}, ops: &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Succeeded")}}},
		{name: "no operation name", app: func(t *testing.T) map[string]any {
			app := jsonObj(t, appWithOperation)
			delete(app["status"].(map[string]any)["operationState"].(map[string]any), "operationName")
			return app
		}, ops: &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Succeeded")}}},
		{name: "enriched", app: func(t *testing.T) map[string]any { return jsonObj(t, appWithOperation) },
			ops: &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Succeeded")}}, enriched: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := TemplateApplication(context.Background(), tc.app(t), tc.ops)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
				return
			case tc.anyErr:
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			status, _ := out["status"].(map[string]any)
			os, _ := status["operationState"].(map[string]any)
			_, hasResources := nestedAny(os, "syncResult", "resources")
			_, hasOperation := os["operation"]
			assert.Equal(t, tc.enriched, hasResources)
			assert.Equal(t, tc.enriched, hasOperation)
		})
	}
}

func TestConvertConditionsRemovesEmptyList(t *testing.T) {
	t.Parallel()
	status := map[string]any{"conditions": []any{
		map[string]any{"type": "Reconciled", "status": "True"},
		map[string]any{"type": "Healthy", "status": "True"},
		map[string]any{"type": "ComparisonError", "status": "False"},
		"not-a-map",
		map[string]any{"status": "True"},
	}}
	convertConditions(status)
	_, present := status["conditions"]
	assert.False(t, present)
}

// TemplateApplication never modifies its input and always returns an independent copy.
func TestTemplateApplicationDoesNotAliasInput(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		phase := rapid.SampledFrom([]string{"", "Running", "Succeeded", "Failed", "Terminating"}).Draw(t, "phase")
		owner := rapid.SampledFrom([]string{"guestbook", "other"}).Draw(t, "owner")
		opPhase := rapid.SampledFrom([]string{"Running", "Succeeded", "Error"}).Draw(t, "opPhase")
		app := map[string]any{}
		_ = json.Unmarshal([]byte(appWithOperation), &app)
		app["status"].(map[string]any)["operationState"].(map[string]any)["phase"] = phase
		before, _ := json.Marshal(app)
		ops := &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture(owner, opPhase)}}
		out, err := TemplateApplication(context.Background(), app, ops)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		after, _ := json.Marshal(app)
		if string(before) != string(after) {
			t.Fatalf("input modified")
		}
		// Mutating the output must not reach the input.
		out["status"].(map[string]any)["operationState"].(map[string]any)["phase"] = "Mutated"
		if app["status"].(map[string]any)["operationState"].(map[string]any)["phase"] != phase {
			t.Fatalf("output aliases input")
		}
		if owner != "guestbook" {
			if _, ok := out["status"].(map[string]any)["operationState"].(map[string]any)["operation"]; ok {
				t.Fatalf("another application's operation was exposed")
			}
		}
	})
}

func nestedAny(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func TestTerminalPhase(t *testing.T) {
	t.Parallel()
	for phase, want := range map[string]bool{
		"": false, "Pending": false, "Running": false, "Terminating": false,
		"Succeeded": true, "Failed": true, "Error": true, "Cancelled": true, "Rejected": true,
	} {
		assert.Equal(t, want, terminalPhase(phase), phase)
	}
}

// A cancelled Operation ends Failed in Argo CD ("Operation terminated"), so catalog triggers
// such as on-sync-failed fire for it; other phases are unchanged.
func TestTemplateApplicationMapsCancelledPhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase, message         string
		wantPhase, wantMessage string
	}{
		{phase: "Cancelled", wantPhase: "Failed", wantMessage: "Operation terminated"},
		{phase: "Cancelled", message: "cancelled by alice", wantPhase: "Failed", wantMessage: "cancelled by alice"},
		{phase: "Rejected", message: "another operation is running", wantPhase: "Rejected", wantMessage: "another operation is running"},
		{phase: "Terminating", wantPhase: "Terminating"},
		{phase: "Failed", message: "one or more objects failed", wantPhase: "Failed", wantMessage: "one or more objects failed"},
	}
	for _, tc := range tests {
		t.Run(tc.phase+"/"+tc.message, func(t *testing.T) {
			t.Parallel()
			os := map[string]any{"phase": tc.phase}
			if tc.message != "" {
				os["message"] = tc.message
			}
			app := map[string]any{"metadata": map[string]any{"name": "a"}, "status": map[string]any{"operationState": os}}
			// Mapped with and without an Operation getter.
			for _, ops := range []OperationGetter{nil, &fakeOps{}} {
				out, err := TemplateApplication(context.Background(), app, ops)
				require.NoError(t, err)
				got := out["status"].(map[string]any)["operationState"].(map[string]any)
				assert.Equal(t, tc.wantPhase, got["phase"])
				msg, _ := got["message"].(string)
				assert.Equal(t, tc.wantMessage, msg)
			}
			assert.Equal(t, tc.phase, os["phase"], "input unchanged")
		})
	}
}
