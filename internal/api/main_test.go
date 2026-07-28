package api

import (
	"os"
	"testing"

	"weaver/internal/testsupport"
)

func TestMain(m *testing.M) {
	os.Exit(testsupport.RunSerialized(m))
}
