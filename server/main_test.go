package server

import (
	"os"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
)

// TestMain initializes the shared HTTP client all fetch paths depend on.
func TestMain(m *testing.M) {
	fetch.Reload()
	os.Exit(m.Run())
}
