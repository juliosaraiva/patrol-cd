package operation

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

const engineServer = "https://a.example.test"

func testEngines(t *testing.T, clk clock.PassiveClock) *KubeEngines {
	t.Helper()
	k, err := NewKubeEngines(KubeEnginesOptions{
		Config: func(_ context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
			return &rest.Config{Host: c.Spec.Server}, nil
		},
		TTL:    time.Minute,
		Clock:  clk,
		Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	return k
}

func engineCluster(server string) *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "one", Generation: 1, ResourceVersion: "10"},
		Spec:       v1alpha1.ClusterSpec{Server: server},
	}
}

func engineFor(t *testing.T, k *KubeEngines, c *v1alpha1.Cluster) *syncpkg.Engine {
	t.Helper()
	e, err := k.Engine(context.Background(), c, "", nil)
	require.NoError(t, err)
	return e
}

func TestKubeEnginesReuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*v1alpha1.Cluster)
		reused bool
	}{
		{name: "nothing changed", change: func(*v1alpha1.Cluster) {}, reused: true},
		{name: "a status write", change: func(c *v1alpha1.Cluster) {
			c.ResourceVersion = "11"
			c.Status.Info.ApplicationsCount = 3
		}, reused: true},
		{name: "a label", change: func(c *v1alpha1.Cluster) {
			c.ResourceVersion = "11"
			c.Labels = map[string]string{"team": "payments"}
		}, reused: true},
		{name: "the spec", change: func(c *v1alpha1.Cluster) {
			c.ResourceVersion = "11"
			c.Generation = 2
		}, reused: false},
		{name: "a recreated object", change: func(c *v1alpha1.Cluster) {
			c.ResourceVersion = "11"
			c.UID = "two"
		}, reused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k := testEngines(t, clock.RealClock{})
			c := engineCluster(engineServer)
			first := engineFor(t, k, c)
			tt.change(c)
			assert.Equal(t, tt.reused, first == engineFor(t, k, c))
		})
	}
}

func TestKubeEnginesForget(t *testing.T) {
	t.Parallel()
	k := testEngines(t, clock.RealClock{})
	c := engineCluster(engineServer)
	other := engineCluster("https://b.example.test")
	first, kept := engineFor(t, k, c), engineFor(t, k, other)
	k.Forget(engineServer + "/")
	assert.NotSame(t, first, engineFor(t, k, c), "credentials can have changed")
	assert.Same(t, kept, engineFor(t, k, other))
}

func TestKubeEnginesDropExpiredEntries(t *testing.T) {
	t.Parallel()
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	k := testEngines(t, clk)
	c := engineCluster(engineServer)
	first := engineFor(t, k, c)
	engineFor(t, k, engineCluster("https://b.example.test"))
	clk.SetTime(clk.Now().Add(time.Minute))
	assert.NotSame(t, first, engineFor(t, k, c), "the TTL bounds how long its credentials are used")
	assert.Len(t, k.engines, 1, "the other expired entry goes with it")
}

func TestKubeEnginesStayBounded(t *testing.T) {
	t.Parallel()
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	k := testEngines(t, clk)
	for i := range maxCachedEngines {
		k.engines[fmt.Sprint(i)] = engineEntry{created: clk.Now()}
	}
	engineFor(t, k, engineCluster(engineServer))
	assert.Len(t, k.engines, maxCachedEngines)
}
