package workflow

import (
	"fmt"
	"strings"
	"testing"
)

// ValidateDef is the only gate between a POSTed document and rows in the database,
// so everything it lets through has to be runnable. These cases are grouped by what
// they protect: the document itself, the edges between tasks, and the per-task
// numbers a run is materialized from.
func TestValidateDefRejects(t *testing.T) {
	cases := []struct {
		name    string
		def     WorkflowDef
		wantErr string // substring, so the message stays diagnosable
	}{
		{
			name:    "no name",
			def:     WorkflowDef{Tasks: []TaskDef{{ID: "a", Handler: "h"}}},
			wantErr: "name is required",
		}, {
			// Trimmed, so a name of spaces is as absent as an empty one.
			name:    "whitespace name",
			def:     WorkflowDef{Name: " \t\n ", Tasks: []TaskDef{{ID: "a", Handler: "h"}}},
			wantErr: "name is required",
		}, {
			name:    "nil task list",
			def:     WorkflowDef{Name: "w"},
			wantErr: "no tasks",
		}, {
			name:    "empty task list",
			def:     WorkflowDef{Name: "w", Tasks: []TaskDef{}},
			wantErr: "no tasks",
		}, {
			name:    "task with no id",
			def:     WorkflowDef{Name: "w", Tasks: []TaskDef{{Handler: "h"}}},
			wantErr: "missing its id",
		}, {
			name:    "task with whitespace id",
			def:     WorkflowDef{Name: "w", Tasks: []TaskDef{{ID: "   ", Handler: "h"}}},
			wantErr: "missing its id",
		}, {
			name: "duplicate task ids",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h"},
				{ID: "a", Handler: "h2"},
			}},
			wantErr: `duplicate task id "a"`,
		}, {
			name:    "task with no handler",
			def:     WorkflowDef{Name: "w", Tasks: []TaskDef{{ID: "a"}}},
			wantErr: "missing its handler",
		}, {
			name:    "task with whitespace handler",
			def:     WorkflowDef{Name: "w", Tasks: []TaskDef{{ID: "a", Handler: " "}}},
			wantErr: "missing its handler",
		}, {
			name: "edge to a task that does not exist",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{"ghost"}},
			}},
			wantErr: `depends on unknown task "ghost"`,
		}, {
			// An empty dependsOn entry names no task, so it is an unknown one rather
			// than something to quietly ignore.
			name: "edge naming the empty string",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{""}},
			}},
			wantErr: "depends on unknown task",
		}, {
			name: "self dependency",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{"a"}},
			}},
			wantErr: `task "a" depends on itself`,
		}, {
			name: "cycle",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{"c"}},
				{ID: "b", Handler: "h", DependsOn: []string{"a"}},
				{ID: "c", Handler: "h", DependsOn: []string{"b"}},
			}},
			wantErr: "cycle detected",
		}, {
			// The same edge twice is a PRIMARY KEY (upstream, downstream) violation in
			// the dependencies table, so letting it through would store a definition
			// that validates and then fails at trigger time with a 500.
			name: "duplicate edge between the same pair",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h"},
				{ID: "b", Handler: "h", DependsOn: []string{"a", "a"}},
			}},
			wantErr: `depends on "a" more than once`,
		}, {
			// Retries counts extra attempts and becomes max_attempts = retries + 1. A
			// negative value yields max_attempts <= 0, which no attempt can satisfy:
			// the first claim already makes attempt 1, so the task dies on its first
			// failure regardless of what it was asked for.
			name: "negative retries",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", Retries: -1},
			}},
			wantErr: "retries cannot be negative",
		}, {
			// A negative timeout becomes a context deadline in the past, so the handler
			// is cancelled before it starts and every attempt "times out".
			name: "negative timeout",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", TimeoutSeconds: -1},
			}},
			wantErr: "timeoutSeconds cannot be negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDef(tc.def)
			if err == nil {
				t.Fatalf("ValidateDef accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateDefAccepts(t *testing.T) {
	cases := []struct {
		name string
		def  WorkflowDef
	}{
		{"diamond", diamondDef()},
		{
			// Zero means "unset" for both, and the store fills in its defaults.
			name: "zero retries and timeout",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a", Handler: "h", Retries: 0, TimeoutSeconds: 0},
			}},
		}, {
			name: "two disconnected chains",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "a1", Handler: "h"},
				{ID: "a2", Handler: "h", DependsOn: []string{"a1"}},
				{ID: "b1", Handler: "h"},
				{ID: "b2", Handler: "h", DependsOn: []string{"b1"}},
			}},
		}, {
			// A task listed before the task it depends on: the document is a set of
			// tasks, not an ordered program.
			name: "dependency declared after the task that uses it",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "b", Handler: "h", DependsOn: []string{"a"}},
				{ID: "a", Handler: "h"},
			}},
		}, {
			name: "ids that are unicode and long",
			def: WorkflowDef{Name: "w", Tasks: []TaskDef{
				{ID: "提取-données-🧵", Handler: "h"},
				{ID: strings.Repeat("x", 4096), Handler: "h", DependsOn: []string{"提取-données-🧵"}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDef(tc.def); err != nil {
				t.Fatalf("ValidateDef rejected a valid definition: %v", err)
			}
		})
	}
}

// A long chain is the case that would blow the stack if the cycle check recursed
// once per task without bound. 50k links is far past any real workflow and still
// has to come back with an answer rather than a crash.
func TestValidateDefHandlesVeryDeepChain(t *testing.T) {
	const depth = 50_000

	tasks := make([]TaskDef, depth)
	tasks[0] = TaskDef{ID: "t0", Handler: "h"}
	for i := 1; i < depth; i++ {
		tasks[i] = TaskDef{
			ID:        fmt.Sprintf("t%d", i),
			Handler:   "h",
			DependsOn: []string{fmt.Sprintf("t%d", i-1)},
		}
	}

	if err := ValidateDef(WorkflowDef{Name: "deep", Tasks: tasks}); err != nil {
		t.Fatalf("a deep but acyclic chain should validate: %v", err)
	}

	// Closing the loop at the far end: the cycle must still be found from the other
	// side of 50k nodes, not just near the root.
	tasks[0].DependsOn = []string{fmt.Sprintf("t%d", depth-1)}
	if err := ValidateDef(WorkflowDef{Name: "deep-cycle", Tasks: tasks}); err == nil {
		t.Fatal("a cycle closed at the end of a deep chain should be rejected")
	}
}
