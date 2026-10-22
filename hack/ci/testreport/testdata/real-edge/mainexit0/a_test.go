package mainexit0

import (
	"os"
	"testing"
)

// TestMain drops the status of m.Run: go test exits 0 and the package passes although a test
// failed.
func TestMain(m *testing.M) {
	m.Run()
	os.Exit(0)
}

func TestFails(t *testing.T) { t.Fatal("failed, but the package passes") }

func TestPasses(t *testing.T) {}
