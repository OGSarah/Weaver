// Package testsupport holds the bits of test setup that have to agree across
// packages. It is imported only from _test.go files.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// lockKey identifies the advisory lock every database-backed test package takes.
// Any constant works as long as all of them use the same one.
const lockKey = int64(0x7765617665720001) // "weaver" + 1

// RunSerialized runs a package's tests holding a database-wide advisory lock, and
// returns the exit code for TestMain.
//
// The tests it guards share one database and one queue. The queue is global by
// design -- ClaimTask takes the next eligible task anywhere in it, not one scoped
// to a run -- so two packages testing against the same database at the same time
// claim each other's tasks and both fail on work that was never theirs. go test
// runs packages concurrently by default, which is exactly that situation.
//
// A session-level advisory lock makes them queue up instead: each package waits
// for the previous one to finish before touching the database. Tests within a
// package already run one at a time, so this is enough to serialize all of them,
// and it holds however the suite is invoked, rather than depending on everyone
// remembering to pass -p 1.
//
// With no DATABASE_URL there is nothing to serialize: the tests skip themselves.
func RunSerialized(m *testing.M) int {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return m.Run()
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: connect: %v\n", err)
		return 1
	}
	defer conn.Close(ctx)

	// Blocks until whichever package holds it is done.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: acquire lock: %v\n", err)
		return 1
	}
	code := m.Run()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey); err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: release lock: %v\n", err)
	}
	return code
}
