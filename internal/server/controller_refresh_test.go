package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// TestClusterCacheRefreshRequested is the regression test of integration finding I-1: the
// cache invalidation request of POST /api/v1/clusters/{server}/invalidate-cache (the
// Cluster's refresh annotation) was ignored by the controller, whose live-state cache was
// never rebuilt.
func TestClusterCacheRefreshRequested(t *testing.T) {
	t.Parallel()
	cluster := func(refresh string) *v1alpha1.Cluster {
		c := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "dest"}, Spec: v1alpha1.ClusterSpec{Server: "https://dest.example", Name: "dest"}}
		if refresh != "" {
			c.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: refresh}
		}
		return c
	}
	const t1, t2 = "2026-10-16T10:00:00Z", "2026-10-16T10:00:05Z"
	for _, tc := range []struct {
		name     string
		ev       clusters.Event
		expected bool
	}{
		{"annotation added", clusters.Event{Type: clusters.EventModified, Old: cluster(""), New: cluster(t1)}, true},
		{"annotation changed", clusters.Event{Type: clusters.EventModified, Old: cluster(t1), New: cluster(t2)}, true},
		{"annotation unchanged (other metadata change)", clusters.Event{Type: clusters.EventModified, Old: cluster(t1), New: cluster(t1)}, false},
		{"annotation removed", clusters.Event{Type: clusters.EventModified, Old: cluster(t1), New: cluster("")}, false},
		{"malformed annotation", clusters.Event{Type: clusters.EventModified, Old: cluster(""), New: cluster("yesterday")}, false},
		{"current state on subscription (served before a restart)", clusters.Event{Type: clusters.EventAdded, New: cluster(t1)}, false},
		{"deleted", clusters.Event{Type: clusters.EventDeleted, Old: cluster(t1)}, false},
		{"modified without previous version", clusters.Event{Type: clusters.EventModified, New: cluster(t1)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, clusterCacheRefreshRequested(tc.ev))
		})
	}
}
