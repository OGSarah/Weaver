package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"weaver/internal/store"
	"weaver/internal/worker"
	"weaver/internal/workflow"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// -seed N inserts a demo run of N independent tasks and exits; without it
	// the process runs the worker loop.
	seed := flag.Int("seed", 0, "seed a demo run with N independent ready tasks, then exit")
	flag.Parse()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	// Cancel on Ctrl-C or SIGTERM (how Compose stops a container) so the worker
	// loop unwinds cleanly instead of being killed mid-poll.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer st.Close()

	if *seed > 0 {
		runID, err := seedDemoRun(ctx, st, *seed)
		if err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Printf("seeded demo run %s with %d independent ready tasks", runID, *seed)
		return
	}

	reg := worker.NewRegistry()
	registerDemoHandlers(reg)

	w := worker.New(workerID(), st, reg)
	// A clean shutdown returns ctx.Canceled; only a real failure is fatal.
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("worker: %v", err)
	}
}

// workerID is a stable-ish identifier for this process. Under Compose each
// scaled replica has its own container hostname, so leases stay attributable to
// the worker that took them.
func workerID() string {
	if id := os.Getenv("WORKER_ID"); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// registerDemoHandlers wires up placeholder handlers that just sleep and log, so
// a worker has something to run before real task code exists. seedDemoRun uses
// "demoTask"; the others match the example workflow in the README.
func registerDemoHandlers(reg *worker.Registry) {
	demo := func(ctx context.Context, task store.ClaimedTask) error {
		// Simulate work of a variable length so two workers visibly interleave.
		d := time.Duration(200+rand.Intn(800)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		log.Printf("handler %q ran task %s (run %s) in %s", task.Handler, task.Name, task.RunID, d)
		return nil
	}

	for _, name := range []string{
		"demoTask",
		"extractData", "transformData", "validateData", "loadWarehouse", "sendEmail",
	} {
		reg.Register(name, demo)
	}
}

// seedDemoRun creates a workflow whose tasks have no dependencies, so every task
// is a root and lands Ready immediately. That gives two workers a pile of
// independent work to divide -- exactly the setup for proving no task is claimed
// twice.
func seedDemoRun(ctx context.Context, st *store.Store, n int) (string, error) {
	tasks := make([]workflow.TaskDef, n)
	for i := range tasks {
		tasks[i] = workflow.TaskDef{
			ID:      fmt.Sprintf("task-%02d", i),
			Handler: "demoTask",
			// Give each task spare attempts so a task stranded by a killed worker
			// is requeued and finished by another, rather than dying on its first
			// (and only) attempt. That is what makes the Phase 6 chaos test show
			// recovery instead of just the attempt-exhaustion path.
			Retries: 3,
		}
	}
	def := workflow.WorkflowDef{
		Name:  fmt.Sprintf("demo-%d", time.Now().Unix()),
		Tasks: tasks,
	}

	workflowID, _, err := st.CreateWorkflow(ctx, def)
	if err != nil {
		return "", err
	}
	return st.CreateRun(ctx, workflowID, def)
}
