package store

import (
	"os"
	"testing"

	"weaver/internal/testsupport"
)

// The database-backed tests in this package share one queue with every other
// package's, so they take a turn rather than running alongside them.
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunSerialized(m))
}
