package clusters

import (
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"
)

// ExecCredentialJSON returns the client.authentication.k8s.io/v1beta1 ExecCredential printed by
// the k8s-auth helpers. A zero expiry is omitted.
func ExecCredentialJSON(tok Token) ([]byte, error) {
	ec := clientauthv1beta1.ExecCredential{
		TypeMeta: metav1.TypeMeta{APIVersion: ExecCredentialAPIVersion, Kind: "ExecCredential"},
		Status:   &clientauthv1beta1.ExecCredentialStatus{Token: tok.Value},
	}
	if !tok.Expiry.IsZero() {
		exp := metav1.NewTime(tok.Expiry.Truncate(time.Second))
		ec.Status.ExpirationTimestamp = &exp
	}
	out, err := json.Marshal(ec)
	if err != nil {
		return nil, fmt.Errorf("encode ExecCredential: %w", err)
	}
	return out, nil
}
