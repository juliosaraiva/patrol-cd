package notifications

import (
	"context"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

var fixedNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

// appFixture builds an Application in unstructured form. Mutators adjust it.
func appFixture(mut ...func(app map[string]any)) map[string]any {
	app := map[string]any{
		"apiVersion": "cd.patrol.example/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":        "guestbook",
			"namespace":   "patrol",
			"labels":      map[string]any{"team": "a"},
			"annotations": map[string]any{},
		},
		"spec": map[string]any{
			"project": "default",
			"source": map[string]any{
				"repoURL":        "https://github.com/example/guestbook.git",
				"path":           "guestbook",
				"targetRevision": "HEAD",
			},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "guestbook"},
		},
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced", "revision": "abc123"},
			"health": map[string]any{"status": "Healthy", "lastTransitionTime": "2026-10-12T11:00:00Z"},
		},
	}
	for _, m := range mut {
		m(app)
	}
	return app
}

func setPath(obj map[string]any, value any, path ...string) {
	cur := obj
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

func withOperation(phase, startedAt, finishedAt, revision string) func(map[string]any) {
	return func(app map[string]any) {
		op := map[string]any{
			"phase":     phase,
			"startedAt": startedAt,
			"operation": map[string]any{"sync": map[string]any{"revision": revision}},
			"syncResult": map[string]any{
				"revision": revision,
			},
		}
		if finishedAt != "" {
			op["finishedAt"] = finishedAt
		}
		setPath(app, op, "status", "operationState")
	}
}

func withAnnotation(k, v string) func(map[string]any) {
	return func(app map[string]any) {
		setPath(app, v, "metadata", "annotations", k)
	}
}

// recordingService captures deliveries.
type recordingService struct {
	mu    sync.Mutex
	sent  []recorded
	fail  error
	calls int
}

type recorded struct {
	n    services.Notification
	dest services.Destination
}

func (r *recordingService) Send(_ context.Context, n services.Notification, d services.Destination) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, recorded{n: n, dest: d})
	return nil
}

func (r *recordingService) deliveries() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.sent...)
}

// fakeRepo implements RepoService.
type fakeRepo struct {
	mu      sync.Mutex
	commits map[string]*CommitMetadata
	calls   int
	details *AppDetail
}

func (f *fakeRepo) GetCommitMetadata(_ context.Context, repoURL, revision, project string) (*CommitMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if m, ok := f.commits[repoURL+"@"+revision]; ok {
		return m, nil
	}
	return nil, errNotFound
}

func (f *fakeRepo) GetAppDetails(context.Context, map[string]any) (*AppDetail, error) {
	if f.details == nil {
		return nil, errNotFound
	}
	return f.details, nil
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errNotFound = testErr("not found")

// fakeProjects implements ProjectGetter.
type fakeProjects map[string]map[string]any

func (f fakeProjects) GetAppProject(_ context.Context, name string) (map[string]any, error) {
	if p, ok := f[name]; ok {
		return p, nil
	}
	return nil, errNotFound
}
