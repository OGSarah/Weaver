package workflow

import (
	"fmt"
	"testing"
)

// diamond: extract fans out to transform and validate, both feeding load.
// The classic case a naive visited-flag cycle check would falsely reject.
func diamondDef() WorkflowDef {
	return WorkflowDef{
		Name: "diamond",
		Tasks: []TaskDef{
			{ID: "extract", Handler: "extractData"},
			{ID: "transform", Handler: "transformData", DependsOn: []string{"extract"}},
			{ID: "validate", Handler: "validateData", DependsOn: []string{"extract"}},
			{ID: "load", Handler: "loadWarehouse", DependsOn: []string{"transform", "validate"}},
		},
	}
}

// The diamond passing Validate is asserted through ValidateDef in
// TestValidateDefAccepts, which calls it: a Validate that rejected a valid fan-out
// would fail there, so a second test of the same fact would only add a place to
// update.

func TestDiamondRoots(t *testing.T) {
	g := NewGraph(diamondDef())

	roots := g.Roots()
	if len(roots) != 1 {
		t.Fatalf("want 1 root, got %d", len(roots))
	}
	if roots[0].ID != "extract" {
		t.Errorf("want root %q, got %q", "extract", roots[0].ID)
	}
}

func TestUnblocks(t *testing.T) {
	g := NewGraph(diamondDef())

	// extract unblocks two tasks.
	got := g.Unblocks("extract")
	if len(got) != 2 {
		t.Fatalf("extract should unblock 2 tasks, got %d", len(got))
	}

	// Order is not guaranteed, so check membership rather than position.
	seen := map[string]bool{}
	for _, task := range got {
		seen[task.ID] = true
		// The full definition comes back, not a bare id: a caller reading
		// Handler off the result must not get a zero value.
		if task.Handler == "" {
			t.Errorf("task %q came back without its handler", task.ID)
		}
	}
	if !seen["transform"] || !seen["validate"] {
		t.Errorf("extract should unblock transform and validate, got %v", seen)
	}

	// load is a leaf: it unblocks nothing, and must not panic.
	if leaf := g.Unblocks("load"); len(leaf) != 0 {
		t.Errorf("load should unblock nothing, got %d", len(leaf))
	}
	// A name no task has is the same shape of answer, not a panic on a nil map.
	if ghost := g.Unblocks("no-such-task"); len(ghost) != 0 {
		t.Errorf("an unknown task should unblock nothing, got %d", len(ghost))
	}
}

func TestTopoSortRespectsDependencies(t *testing.T) {
	g := NewGraph(diamondDef())

	order, err := g.TopoSort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("want 4 tasks in order, got %d", len(order))
	}

	// Record each task's position so we can assert relative ordering.
	// Multiple orderings are valid, so we check constraints, not exact sequence.
	pos := map[string]int{}
	for i, task := range order {
		pos[task.ID] = i
	}

	if pos["extract"] > pos["transform"] {
		t.Error("extract must come before transform")
	}
	if pos["extract"] > pos["validate"] {
		t.Error("extract must come before validate")
	}
	if pos["transform"] > pos["load"] {
		t.Error("transform must come before load")
	}
	if pos["validate"] > pos["load"] {
		t.Error("validate must come before load")
	}
}

// TopoSort walks maps internally, and Go randomizes map iteration order. The
// ordering must come from the task list and the edges, so the same definition has
// to produce the same sequence every time rather than one that drifts between runs
// and makes a failure elsewhere unreproducible.
func TestTopoSortIsDeterministic(t *testing.T) {
	first, err := NewGraph(diamondDef()).TopoSort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 0; i < 20; i++ {
		again, err := NewGraph(diamondDef()).TopoSort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for j := range first {
			if again[j].ID != first[j].ID {
				t.Fatalf("run %d ordered %q at position %d, first run had %q",
					i, again[j].ID, j, first[j].ID)
			}
		}
	}
}

// Cycles are the one structural property that makes a workflow unrunnable, and
// each of these shapes defeats a different naive implementation: a visited-flag
// check, a DFS seeded only from roots, or one that stops at the first component.
func TestCyclesAreRejected(t *testing.T) {
	cases := []struct {
		name  string
		tasks []TaskDef
	}{
		{
			name: "two nodes pointing at each other",
			tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{"b"}},
				{ID: "b", Handler: "h", DependsOn: []string{"a"}},
			},
		}, {
			name:  "task depending on itself",
			tasks: []TaskDef{{ID: "a", Handler: "h", DependsOn: []string{"a"}}},
		}, {
			name: "three node loop",
			tasks: []TaskDef{
				{ID: "a", Handler: "h", DependsOn: []string{"c"}},
				{ID: "b", Handler: "h", DependsOn: []string{"a"}},
				{ID: "c", Handler: "h", DependsOn: []string{"b"}},
			},
		}, {
			// No root leads to x and y, so a DFS seeded only from Roots() never
			// reaches them and reports a clean graph.
			name: "cycle in a component with no root",
			tasks: []TaskDef{
				{ID: "a1", Handler: "h"},
				{ID: "x", Handler: "h", DependsOn: []string{"y"}},
				{ID: "y", Handler: "h", DependsOn: []string{"x"}},
			},
		}, {
			// The traversal reaches a fully explored node (done) on its way to the
			// loop. Treating "seen before" as "cycle" would reject valid graphs;
			// treating it as "safe to skip" must not hide this one.
			name: "cycle behind a valid prefix",
			tasks: []TaskDef{
				{ID: "root", Handler: "h"},
				{ID: "a", Handler: "h", DependsOn: []string{"root", "c"}},
				{ID: "b", Handler: "h", DependsOn: []string{"a"}},
				{ID: "c", Handler: "h", DependsOn: []string{"b"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGraph(WorkflowDef{Name: tc.name, Tasks: tc.tasks})

			if err := g.Validate(); err == nil {
				t.Error("Validate accepted a cyclic graph")
			}
			// The two answers must agree: an ordering cannot exist for a graph
			// Validate rejects, and a caller that skipped Validate must still be
			// stopped here rather than getting a partial order back.
			order, err := g.TopoSort()
			if err == nil {
				t.Errorf("TopoSort returned an order for a cyclic graph: %v", order)
			}
			if order != nil {
				t.Errorf("TopoSort returned %d tasks alongside its error", len(order))
			}
		})
	}
}

// A cycle has no task with an empty DependsOn, so a run built from one would have
// nothing to start. Roots reporting that honestly is what makes the emptiness
// visible instead of a run that hangs with no explanation.
func TestCycleHasNoRoots(t *testing.T) {
	g := NewGraph(WorkflowDef{Name: "cycle", Tasks: []TaskDef{
		{ID: "a", Handler: "h", DependsOn: []string{"b"}},
		{ID: "b", Handler: "h", DependsOn: []string{"a"}},
	}})

	if roots := g.Roots(); len(roots) != 0 {
		t.Errorf("a cycle should have no roots, got %d", len(roots))
	}
}

func TestDisconnectedGraph(t *testing.T) {
	def := WorkflowDef{
		Name: "disconnected",
		Tasks: []TaskDef{
			{ID: "a1", Handler: "h"},
			{ID: "a2", Handler: "h", DependsOn: []string{"a1"}},
			{ID: "b1", Handler: "h"},
			{ID: "b2", Handler: "h", DependsOn: []string{"b1"}},
		},
	}
	g := NewGraph(def)

	if err := g.Validate(); err != nil {
		t.Fatalf("disconnected but acyclic graph should be valid, got: %v", err)
	}

	// Both clusters contribute a root.
	if roots := g.Roots(); len(roots) != 2 {
		t.Errorf("want 2 roots, got %d", len(roots))
	}

	order, err := g.TopoSort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 {
		t.Errorf("all 4 tasks should be ordered, got %d", len(order))
	}
}

// An empty definition is degenerate rather than invalid at this layer (ValidateDef
// is what rejects it), so every method has to answer for it without panicking on a
// nil map or a zero-length slice.
func TestEmptyGraph(t *testing.T) {
	g := NewGraph(WorkflowDef{Name: "empty"})

	if err := g.Validate(); err != nil {
		t.Errorf("an empty graph has no cycle, got: %v", err)
	}
	if roots := g.Roots(); len(roots) != 0 {
		t.Errorf("want no roots, got %d", len(roots))
	}
	order, err := g.TopoSort()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(order) != 0 {
		t.Errorf("want no ordering, got %d entries", len(order))
	}
	if got := g.Unblocks("anything"); len(got) != 0 {
		t.Errorf("want nothing unblocked, got %d", len(got))
	}
}

// A wide graph is the other shape of large: one task every other task waits on.
// It exercises the fan-out indexes rather than the recursion depth that
// TestValidateDefHandlesVeryDeepChain covers.
func TestWideFanOutOrdersEveryTask(t *testing.T) {
	const width = 5_000

	tasks := []TaskDef{{ID: "root", Handler: "h"}}
	for i := 0; i < width; i++ {
		tasks = append(tasks, TaskDef{
			ID:        fmt.Sprintf("leaf-%d", i),
			Handler:   "h",
			DependsOn: []string{"root"},
		})
	}
	g := NewGraph(WorkflowDef{Name: "wide", Tasks: tasks})

	if err := g.Validate(); err != nil {
		t.Fatalf("a fan-out is acyclic: %v", err)
	}
	if got := len(g.Unblocks("root")); got != width {
		t.Errorf("root should unblock %d tasks, got %d", width, got)
	}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != width+1 {
		t.Fatalf("want %d tasks ordered, got %d", width+1, len(order))
	}
	if order[0].ID != "root" {
		t.Errorf("the only task with no upstream must be ordered first, got %q", order[0].ID)
	}
}
