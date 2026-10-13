package cache

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authzclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// checkAccess asks the API server whether the cache's credentials may perform verb on api
// in namespace ns ("" = all namespaces / cluster scope).
func checkAccess(ctx context.Context, ssar authzclient.SelfSubjectAccessReviewInterface, api APIResourceInfo, ns, verb string) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns,
				Verb:      verb,
				Group:     api.GroupKind.Group,
				Resource:  api.GroupVersionResource.Resource,
			},
		},
	}
	resp, err := ssar.Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("self subject access review for %s %s: %w", verb, api.GroupVersionResource.String(), err)
	}
	return resp.Status.Allowed, nil
}
