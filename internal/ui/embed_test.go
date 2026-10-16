package ui

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFS checks both states of the embedded tree: a copied UI build (make ui-build) is
// served as is; without one, the placeholder page explains how to build it.
func TestFS(t *testing.T) {
	t.Parallel()
	fsys, built := FS()
	index, err := fs.ReadFile(fsys, "index.html")
	require.NoError(t, err)
	_, statErr := fs.Stat(dist, "dist/index.html")
	assert.Equal(t, statErr == nil, built)
	if built {
		assert.NotContains(t, string(index), "is not built into this binary")
		return
	}
	assert.Contains(t, string(index), "Patrol CD web UI is not built into this binary")
	assert.Contains(t, string(index), "make ui-build")
}
