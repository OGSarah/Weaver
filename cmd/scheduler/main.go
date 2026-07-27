package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"weaver/internal/scheduler"
	"weaver/internal/store"

	"golang.org/x/sync/errgroup"
)

// The scheduler is the control-plane process. It runs two loops against the shared
// database: the reaper, which recovers tasks orphaned by dead workers, and the
// cron scheduler, which creates runs when a workflow's schedule comes due. Both are
// safe to run in several replicas -- SKIP LOCKED and the per-slot unique index keep
// them from stepping on each other -- but one replica is enough.
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	// Cancel on Ctrl-C or SIGTERM (how Compose stops a container) so both loops
	// unwind cleanly instead of being killed mid-sweep.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer st.Close()

	// Run the reaper and the scheduler concurrently. If either returns a real
	// error the group's context is cancelled, stopping the other too.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return scheduler.NewReaper(st).Run(gctx) })
	g.Go(func() error { return scheduler.NewScheduler(st).Run(gctx) })

	// A clean shutdown cancels ctx, which surfaces as context.Canceled from both
	// loops; only a genuine failure is fatal.
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("scheduler: %v", err)
	}
}
