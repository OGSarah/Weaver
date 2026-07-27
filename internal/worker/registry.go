package worker

import (
	"context"

	"weaver/internal/store"
)

// HandlerFunc is the code a task runs. The name in a task's definition is looked
// up in the Registry to find the matching function. The ctx carries
// cancellation (Phase 5 uses it to enforce per-task timeouts); the task gives
// the handler its identity, so an idempotent handler can key off RunID + ID.
type HandlerFunc func(ctx context.Context, task store.ClaimedTask) error

// Registry maps a handler name to the Go function that implements it. It is
// populated once at startup and only read afterward, so it needs no locking.
type Registry struct {
	handlers map[string]HandlerFunc
}

// NewRegistry returns an empty registry ready for Register calls.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]HandlerFunc)}
}

// Register binds a handler name to its function. A later Register with the same
// name replaces the earlier one.
func (r *Registry) Register(name string, fn HandlerFunc) {
	r.handlers[name] = fn
}

// Lookup returns the handler for a name and whether one was registered. A task
// naming an unregistered handler is a definition bug, not something to run.
func (r *Registry) Lookup(name string) (HandlerFunc, bool) {
	fn, ok := r.handlers[name]
	return fn, ok
}
