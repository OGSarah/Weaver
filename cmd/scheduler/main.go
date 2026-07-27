package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"weaver/internal/scheduler"
	"weaver/internal/store"
)

// The scheduler is the control-plane process. For now it runs the reaper, which
// recovers tasks orphaned by dead workers; Phase 7 adds cron-style triggering of
// due workflows to this same binary.
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	// Cancel on Ctrl-C or SIGTERM (how Compose stops a container) so the reaper
	// loop unwinds cleanly instead of being killed mid-sweep.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer st.Close()

	r := scheduler.NewReaper(st)
	// A clean shutdown returns ctx.Canceled; only a real failure is fatal.
	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("reaper: %v", err)
	}
}
