package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestIndexKeys(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: "https://k8s", Namespace: "web"}}}
	assert.Equal(t, []string{"web"}, destinationNamespaceKeys(app))
	assert.Nil(t, destinationNamespaceKeys(&v1alpha1.Application{}), "no destination namespace")
	assert.Nil(t, destinationNamespaceKeys(&v1alpha1.AppProject{}), "not an Application")

	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	assert.Nil(t, orphanMonitoringKeys(proj), "monitoring off")
	proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
	assert.Equal(t, []string{orphanMonitoringKey}, orphanMonitoringKeys(proj), "an empty orphanedResources enables monitoring")
	assert.Nil(t, orphanMonitoringKeys(app), "not an AppProject")
}
