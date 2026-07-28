package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"weaver/internal/store"
)

func TestRegistry(t *testing.T) {
	reg := NewRegistry()

	if _, ok := reg.Lookup("nothing"); ok {
		t.Error("an empty registry should know no handlers")
	}

	called := ""
	reg.Register("a", func(context.Context, store.ClaimedTask, *TaskLogger) error {
		called = "first"
		return nil
	})
	// A later Register with the same name replaces the earlier one, which is what
	// makes registration order the deciding factor rather than an error nobody
	// sees at startup.
	reg.Register("a", func(context.Context, store.ClaimedTask, *TaskLogger) error {
		called = "second"
		return nil
	})

	fn, ok := reg.Lookup("a")
	if !ok {
		t.Fatal("registered handler not found")
	}
	if err := fn(context.Background(), store.ClaimedTask{}, nil); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if called != "second" {
		t.Errorf("lookup returned the %s registration, want the second", called)
	}

	// Handler names come from a stored definition, so they can be anything a JSON
	// document can hold. Nothing about lookup should treat these specially.
	for _, name := range []string{"", " ", "handler with spaces", "🧵", strings.Repeat("x", 4096)} {
		reg.Register(name, func(context.Context, store.ClaimedTask, *TaskLogger) error { return nil })
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("handler %q was registered but not found", name)
		}
	}
	// Lookup must not be fooled by a name that merely resembles a registered one.
	if _, ok := reg.Lookup("A"); ok {
		t.Error("lookup should be case sensitive")
	}
	if _, ok := reg.Lookup("a "); ok {
		t.Error("lookup should not trim its argument")
	}
}

// A panicking handler must not take the worker process down with it: the whole
// pool would die on one bad task, and every other task it was running would be
// abandoned to the reaper.
func TestRunHandlerConvertsPanicToError(t *testing.T) {
	cases := []struct {
		name    string
		handler HandlerFunc
		wantErr string
	}{
		{
			name: "panic with a string",
			handler: func(context.Context, store.ClaimedTask, *TaskLogger) error {
				panic("something went very wrong")
			},
			wantErr: "something went very wrong",
		}, {
			name: "panic with an error",
			handler: func(context.Context, store.ClaimedTask, *TaskLogger) error {
				panic(errors.New("boom"))
			},
			wantErr: "boom",
		}, {
			// A nil dereference inside a handler is the panic that actually happens
			// in practice, and it has to be caught like any other.
			name: "nil map write",
			handler: func(context.Context, store.ClaimedTask, *TaskLogger) error {
				var m map[string]string
				m["k"] = "v"
				return nil
			},
			wantErr: "nil map",
		}, {
			// panic(nil) became a runtime error in Go 1.21 rather than a recover
			// that returns nil, so this must still be reported as a failure and not
			// silently pass as a success.
			name: "panic with nil",
			handler: func(context.Context, store.ClaimedTask, *TaskLogger) error {
				panic(nil)
			},
			wantErr: "panicked",
		}, {
			// A handler that logs and then panics: the panic must still be the
			// reported outcome.
			name: "panic after returning a value is impossible, panic in defer is not",
			handler: func(context.Context, store.ClaimedTask, *TaskLogger) (err error) {
				defer func() { panic("from the defer") }()
				return errors.New("original error")
			},
			wantErr: "from the defer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runHandler(context.Background(), tc.handler, store.ClaimedTask{}, nil)
			if err == nil {
				t.Fatal("a panicking handler should produce an error")
			}
			if !strings.Contains(err.Error(), "panicked") {
				t.Errorf("error %q does not say the handler panicked", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not carry %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunHandlerPassesThroughOrdinaryOutcomes(t *testing.T) {
	if err := runHandler(context.Background(),
		func(context.Context, store.ClaimedTask, *TaskLogger) error { return nil },
		store.ClaimedTask{}, nil); err != nil {
		t.Errorf("a clean handler should return nil, got %v", err)
	}

	sentinel := errors.New("handler said no")
	err := runHandler(context.Background(),
		func(context.Context, store.ClaimedTask, *TaskLogger) error { return sentinel },
		store.ClaimedTask{}, nil)
	// Wrapped or not, the caller has to be able to recognise the original error.
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want the handler's own error", err)
	}
}

// The handler receives the task it was claimed for. An idempotent handler keys off
// RunID and ID, so passing the wrong one would make deduplication silently wrong.
func TestRunHandlerReceivesTheClaimedTask(t *testing.T) {
	want := store.ClaimedTask{ID: "task-1", RunID: "run-1", Name: "extract", Attempt: 2, MaxAttempts: 3}

	var got store.ClaimedTask
	err := runHandler(context.Background(),
		func(_ context.Context, task store.ClaimedTask, _ *TaskLogger) error {
			got = task
			return nil
		}, want, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("handler got %+v, want %+v", got, want)
	}
}

// wait is what a shutdown has to interrupt: a worker sitting on a poll interval
// must not keep the process alive for the rest of it.
func TestWaitReturnsEarlyOnCancel(t *testing.T) {
	w := &Worker{ID: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	w.wait(ctx, 10*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %s on a cancelled context; it should return at once", elapsed)
	}
}

func TestWaitSleepsWhenNotCancelled(t *testing.T) {
	w := &Worker{ID: "test"}

	start := time.Now()
	w.wait(context.Background(), 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("wait returned after %s, want about 50ms", elapsed)
	}
}
