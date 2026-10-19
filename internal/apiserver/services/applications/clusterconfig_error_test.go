package applications

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type userFacingErr struct{ text string }

func (e *userFacingErr) Error() string       { return "internal: " + e.text }
func (e *userFacingErr) UserMessage() string { return e.text }

func TestClusterConfigUnavailable(t *testing.T) {
	t.Parallel()
	const server = "https://dest.example:6443"

	t.Run("a provider error without UserMessage stays out of the response", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("credentials secret ns/cluster-dest of cluster dest: not found (token=hunter2)")
		e := clusterConfigUnavailable(server, cause)
		assert.Equal(t, http.StatusServiceUnavailable, e.Status)
		assert.Contains(t, e.Detail, "the configuration of cluster "+server+" is not available")
		assert.NotContains(t, e.Detail, "hunter2", "Detail is what the client receives")
		assert.NotContains(t, e.Detail, "credentials secret")
		assert.ErrorIs(t, e, cause, "the cause stays attached for the log")
	})

	t.Run("a UserMessage error is shown, also when wrapped", func(t *testing.T) {
		t.Parallel()
		cause := fmt.Errorf("resolve: %w", &userFacingErr{text: "core mode: no kubeconfig context for destination cluster " + server})
		e := clusterConfigUnavailable(server, cause)
		assert.Equal(t, http.StatusServiceUnavailable, e.Status)
		require.Contains(t, e.Detail, "is not available: core mode: no kubeconfig context for destination cluster "+server)
		assert.NotContains(t, e.Detail, "internal:", "only UserMessage is shown, not the error text")
	})
}
