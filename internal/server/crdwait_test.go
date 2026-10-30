package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	krest "k8s.io/client-go/rest"
)

func TestPatrolResourceKinds(t *testing.T) {
	t.Parallel()
	s, err := newScheme()
	require.NoError(t, err)
	assert.Equal(t, []string{
		"AppProject", "Application", "ApplicationSet", "Cluster", "Operation",
		"Repository", "RepositoryCredential", "SCMProvider",
	}, patrolResourceKinds(s))
}

// discoveryServer answers GET /apis/cd.patrol.example/v1alpha1 with respond(n) for the n-th request
// (counting from 1): nil means 404, otherwise the kinds listed.
func discoveryServer(t *testing.T, respond func(n int) []string) (*krest.Config, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1))
		if r.URL.Path != "/apis/cd.patrol.example/v1alpha1" {
			http.NotFound(w, r)
			return
		}
		kinds := respond(i)
		if kinds == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status:   metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
			})
			return
		}
		list := metav1.APIResourceList{
			TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
			GroupVersion: "cd.patrol.example/v1alpha1",
		}
		for _, k := range kinds {
			list.APIResources = append(list.APIResources, metav1.APIResource{Name: k + "s", Kind: k, Namespaced: true, Verbs: metav1.Verbs{"get"}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	return &krest.Config{Host: srv.URL}, &n
}

var allPatrolKinds = []string{
	"AppProject", "Application", "ApplicationSet", "Cluster", "Operation",
	"Repository", "RepositoryCredential", "SCMProvider",
}

// expiringContext is a parent context whose deadline passes when the test closes expired, not on
// a clock. waitForCRDs derives its own context.WithTimeout from it, and the context package
// cancels a child with the parent's Err once the parent is done, so the wait sees
// context.DeadlineExceeded and takes its timeout path exactly when the test decides. The tests
// pass crdWaitTimeout as the wait's own timeout, which does not fire first.
type expiringContext struct {
	context.Context
	expired <-chan struct{}
}

func (c expiringContext) Done() <-chan struct{} { return c.expired }

func (c expiringContext) Err() error {
	select {
	case <-c.expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// newExpiry returns a context whose deadline passes when expire is called (once or more).
func newExpiry() (ctx context.Context, expire func()) {
	expired := make(chan struct{})
	return expiringContext{Context: context.Background(), expired: expired}, sync.OnceFunc(func() { close(expired) })
}

func TestWaitForCRDsReturnsOnceEveryKindIsServed(t *testing.T) {
	t.Parallel()
	cfg, n := discoveryServer(t, func(n int) []string {
		switch {
		case n <= 2:
			return nil // the group is not served yet
		case n == 3:
			return allPatrolKinds[:5] // some CRDs established, not all
		default:
			return allPatrolKinds
		}
	})
	start := time.Now()
	require.NoError(t, waitForCRDs(t.Context(), cfg, slog.New(slog.DiscardHandler), 30*time.Second, 5*time.Millisecond))
	assert.EqualValues(t, 4, n.Load())
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestWaitForCRDsNoWaitWhenServed(t *testing.T) {
	t.Parallel()
	cfg, n := discoveryServer(t, func(int) []string { return allPatrolKinds })
	require.NoError(t, waitForCRDs(t.Context(), cfg, slog.New(slog.DiscardHandler), time.Minute, time.Hour))
	assert.EqualValues(t, 1, n.Load())
}

// The deadline passes during the second attempt, after the first answer: the error names the
// kinds that answer lacked, and the interrupted attempt changes nothing (CI-F13). Issue #228: with
// a 100 ms deadline, a loaded machine got no answer in time and every kind was reported missing.
func TestWaitForCRDsTimesOutNamingTheMissingKinds(t *testing.T) {
	t.Parallel()
	ctx, expire := newExpiry()
	stop := make(chan struct{})
	cfg, n := discoveryServer(t, func(n int) []string {
		if n > 1 {
			expire()
			<-stop // never answered: the attempt is cut short by the deadline
		}
		return []string{"Application", "AppProject"}
	})
	t.Cleanup(func() { close(stop) }) // runs before the server's Close, which waits for the handler
	err := waitForCRDs(ctx, cfg, slog.New(slog.DiscardHandler), crdWaitTimeout, 10*time.Millisecond)
	require.EqualError(t, err, "the Patrol CRDs (cd.patrol.example/v1alpha1) are not served after 1m0s: "+
		"missing ApplicationSet, Cluster, Operation, Repository, RepositoryCredential, SCMProvider")
	assert.GreaterOrEqual(t, n.Load(), int32(2))
}

// The last real error survives an attempt the deadline interrupted.
func TestWaitForCRDsKeepsTheLastDiscoveryError(t *testing.T) {
	t.Parallel()
	ctx, expire := newExpiry()
	stop := make(chan struct{})
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) > 1 {
			expire()
			<-stop
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // runs before srv.Close
	err := waitForCRDs(ctx, &krest.Config{Host: srv.URL}, slog.New(slog.DiscardHandler), crdWaitTimeout, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not served after "+crdWaitTimeout.String())
	assert.Contains(t, err.Error(), "boom")
	assert.NotContains(t, err.Error(), "context", "the interrupted attempt's error is not reported")
}

func TestWaitForCRDsStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	cfg, _ := discoveryServer(t, func(int) []string { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := waitForCRDs(ctx, cfg, slog.New(slog.DiscardHandler), time.Minute, 10*time.Millisecond)
	require.ErrorIs(t, err, context.Canceled)
}

// The deadline passes while a probe hangs: the probe ends with the wait.
func TestWaitForCRDsDoesNotWaitForAHangingProbe(t *testing.T) {
	t.Parallel()
	ctx, expire := newExpiry()
	expiredAt := make(chan time.Time, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		select {
		case expiredAt <- time.Now():
		default:
		}
		expire()
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	err := waitForCRDs(ctx, &krest.Config{Host: srv.URL}, slog.New(slog.DiscardHandler), crdWaitTimeout, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not served after "+crdWaitTimeout.String())
	select {
	case at := <-expiredAt:
		assert.Less(t, time.Since(at), crdProbeTimeout/2, "the request in flight must end with the wait, not after its own 10 s timeout")
	default:
		t.Fatal("no probe reached the server")
	}
}

// startRecorder returns a component whose run records that it was called.
func startRecorder(kube bool) (*component, *atomic.Bool) {
	var ran atomic.Bool
	return &component{
		flags: commonFlags{kube: kube},
		run:   func(context.Context, *Runtime) error { ran.Store(true); return nil },
	}, &ran
}

func TestStartWaitsForTheCRDsOfKubeComponentsOnly(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)

	t.Run("served: runs after the check", func(t *testing.T) {
		t.Parallel()
		cfg, n := discoveryServer(t, func(int) []string { return allPatrolKinds })
		c, ran := startRecorder(true)
		require.NoError(t, c.start(t.Context(), &Runtime{Rest: cfg, Log: log}, time.Now()))
		assert.True(t, ran.Load())
		assert.EqualValues(t, 1, n.Load())
	})
	t.Run("not served: does not run, a stop is clean", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// The stop arrives while discovery says the CRDs are not served.
		cfg, n := discoveryServer(t, func(int) []string { cancel(); return nil })
		c, ran := startRecorder(true)
		require.NoError(t, c.start(ctx, &Runtime{Rest: cfg, Log: log}, time.Now()))
		assert.False(t, ran.Load(), "the component must not start before its CRDs are served")
		assert.Positive(t, n.Load())
	})
	t.Run("the budget counts the time bootstrap took", func(t *testing.T) {
		t.Parallel()
		cfg, _ := discoveryServer(t, func(int) []string { return nil })
		c, ran := startRecorder(true)
		began := time.Now().Add(-crdWaitTimeout) // a minute already spent: one interval is left
		start := time.Now()
		err := c.start(t.Context(), &Runtime{Rest: cfg, Log: log}, began)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not served after "+crdWaitInterval.String()+":", "the wait gets one interval, not the full timeout")
		assert.False(t, ran.Load())
		// Generous: the wait ends at its own deadline, long before the full timeout.
		assert.Less(t, time.Since(start), crdWaitTimeout/2)
	})
	t.Run("no Kubernetes access: runs without a check", func(t *testing.T) {
		t.Parallel()
		c, ran := startRecorder(true)
		require.NoError(t, c.start(t.Context(), &Runtime{Log: log}, time.Now()))
		assert.True(t, ran.Load())
	})
	t.Run("component without the kube flag: runs without a check", func(t *testing.T) {
		t.Parallel()
		cfg, n := discoveryServer(t, func(int) []string { return nil })
		c, ran := startRecorder(false)
		require.NoError(t, c.start(t.Context(), &Runtime{Rest: cfg, Log: log}, time.Now()))
		assert.True(t, ran.Load())
		assert.Zero(t, n.Load())
	})
}

// TestKubeFlagsOfTheCommands pins which commands get the wait: the ones that use the control-plane
// API (they register --kubeconfig through the kube flag), and none of the others.
func TestKubeFlagsOfTheCommands(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cmd  *cobra.Command
		kube bool
	}{
		"server":                   {newServerCommand(), true},
		"controller":               {newControllerCommand(), true},
		"appset-controller":        {newAppSetControllerCommand(), true},
		"notifications-controller": {newNotificationsControllerCommand(), true},
		"repo-server":              {newRepoServerCommand(), false},
		"commit-server":            {newCommitServerCommand(), false},
		"cmp-server":               {newCMPServerCommand(), false},
	} {
		assert.Equal(t, tc.kube, tc.cmd.Flags().Lookup("kubeconfig") != nil, name)
	}
}
