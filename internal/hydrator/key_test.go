package hydrator

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// APP-116, APP-123, APP-124: the grouping key normalizes repository URLs and uses the
// hydrateTo branch when set.
func TestKeyOf(t *testing.T) {
	t.Parallel()
	_, ok := KeyOf(&v1alpha1.Application{})
	assert.False(t, ok)
	_, ok = KeyOf(nil)
	assert.False(t, ok)

	a := hydratorApp("a", "out/a")
	ka, ok := KeyOf(a)
	require.True(t, ok)
	assert.Equal(t, Key{
		SourceRepoURL:        "https://git.example.com/org/dry",
		SourceTargetRevision: "main",
		DestinationRepoURL:   "https://git.example.com/org/dry",
		DestinationBranch:    "env/prod",
	}, ka)

	b := hydratorApp("b", "out/b")
	b.Spec.SourceHydrator.DrySource.RepoURL = "HTTPS://git.example.com/org/dry"
	b.Spec.SourceHydrator.SyncSource.RepoURL = "https://git.example.com/org/dry.git"
	assert.Equal(t, ka, keyOf(t, b), "different spellings of one repository share a key")
	assert.Equal(t, ka.indexValue(), keyOf(t, b).indexValue())

	c := hydratorApp("c", "out/c")
	c.Spec.SourceHydrator.HydrateTo = &v1alpha1.HydrateTo{TargetBranch: "env/prod-next"}
	kc := keyOf(t, c)
	assert.Equal(t, "env/prod-next", kc.DestinationBranch)
	assert.NotEqual(t, ka.indexValue(), kc.indexValue())

	d := hydratorApp("d", "out/d")
	d.Spec.SourceHydrator.SyncSource.RepoURL = "git@git.example.com:org/hydrated.git"
	assert.Equal(t, "git@git.example.com/org/hydrated", keyOf(t, d).DestinationRepoURL)

	assert.Equal(t, []string{ka.indexValue()}, indexByKey(a))
	assert.Nil(t, indexByKey(&v1alpha1.Application{}))
	assert.Nil(t, indexByKey(&v1alpha1.AppProject{}))
}

func TestPathChecks(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]bool{"": true, ".": true, "/": true, "./": true, "a/..": true, "a": false, "./a": false} {
		assert.Equal(t, want, isRootPath(p), "isRootPath(%q)", p)
	}
	for p, want := range map[string]bool{"..": true, "../a": true, "a/../..": true, "/abs": true, "a": false, "a/../b": false, "..a": false} {
		assert.Equal(t, want, isOutsideRepository(p), "isOutsideRepository(%q)", p)
	}
}

func TestRefreshPaths(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"apps/a", "apps/common", "shared"}, refreshPaths(" . ; ../common;;/shared ", "apps/a"))
	assert.Nil(t, refreshPaths("", "apps/a"))
}

// Messages stored in the status are bounded and never carry URL passwords.
func TestStatusMessage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "failed to clone https://bot:***@git.example.com/org/repo.git: denied",
		statusMessage("failed to clone https://bot:hunter2@git.example.com/org/repo.git: denied"))
	assert.Equal(t, "ssh://git@host/x and https://host/y", statusMessage("ssh://git@host/x and https://host/y"))
	long := strings.Repeat("é", maxStatusMessageRunes+10)
	got := statusMessage(long)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, maxStatusMessageRunes+3, utf8.RuneCountInString(got))
}

// Keys added before the controller starts are buffered, deduplicated and flushed on Start.
func TestKeyQueueBuffersUntilStart(t *testing.T) {
	t.Parallel()
	s := newKeyQueue()
	k1, k2 := Key{SourceRepoURL: "a"}, Key{SourceRepoURL: "b"}
	s.add(k1)
	s.add(k1)
	s.add(k2)
	assert.Len(t, s.pendingKeys(), 2)

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[Key]())
	defer q.ShutDown()
	require.NoError(t, s.Start(context.Background(), q))
	assert.Equal(t, 2, q.Len())
	s.add(Key{SourceRepoURL: "c"})
	s.add(k1)
	assert.Equal(t, 3, q.Len(), "the workqueue deduplicates pending keys")
	assert.Empty(t, s.pendingKeys())
	assert.Equal(t, "hydration-keys", s.String())
}

func TestNewValidatesDeps(t *testing.T) {
	t.Parallel()
	_, err := New(Deps{})
	require.Error(t, err)
	for _, name := range []string{"Client", "RepoServer", "CommitServer", "Repositories", "WriteCredentials", "Settings"} {
		assert.ErrorContains(t, err, name+" is required")
	}
	h, err := Setup(context.Background(), nil, Deps{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, h, "hydrator.enabled=false registers nothing")
	_, err = Setup(context.Background(), nil, Deps{Enabled: true})
	assert.ErrorContains(t, err, "manager is required")
}

func TestProcessorsDefaults(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	assert.Equal(t, DefaultProcessors, e.h.d.Processors)
	assert.Equal(t, 1, e.h.d.AppProcessors)

	d := e.h.d
	d.Processors = -3
	require.NoError(t, d.normalize())
	assert.Equal(t, 1, d.Processors, "values below 1 are coerced to 1 (APP-136)")
}
