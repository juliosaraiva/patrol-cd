package admin

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// lazyRESTMapper returns a discovery-backed REST mapper that discovers groups on first use.
func lazyRESTMapper(cfg *rest.Config, httpClient *http.Client) (meta.RESTMapper, error) {
	m, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("could not create REST mapper: %w", err)
	}
	return m, nil
}
