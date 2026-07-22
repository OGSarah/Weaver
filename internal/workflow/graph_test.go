package workflow

import "testing"

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

func TestDiamondIsValid(t *testing.T) {
	g := NewGraph(diamondDef())

	if err := g.Validate(); err != nil {
		t.Fatalf("diamond should be a valid DAG, got error: %v", err)
	}
}

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
	}
	if !seen["transform"] || !seen["validate"] {
		t.Errorf("extract should unblock transform and validate, got %v", seen)
	}

	// load is a leaf: it unblocks nothing, and must not panic.
	if leaf := g.Unblocks("load"); len(leaf) != 0 {
		t.Errorf("load should unblock nothing, got %d", len(leaf))
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

// a -> b -> a
func TestSimpleCycleIsRejected(t *testing.T) {
	def := WorkflowDef{
		Name: "simple-cycle",
		Tasks: []TaskDef{
			{ID: "a", Handler: "h", DependsOn: []string{"b"}},
			{ID: "b", Handler: "h", DependsOn: []string{"a"}},
		},
	}
	g := NewGraph(def)

	if err := g.Validate(); err == nil {
		t.Error("Validate should reject a two-node cycle")
	}
	if _, err := g.TopoSort(); err == nil {
		t.Error("TopoSort should reject a two-node cycle")
	}
	// A cyclic graph has no task with an empty DependsOn.
	if roots := g.Roots(); len(roots) != 0 {
		t.Errorf("a cycle should have no roots, got %d", len(roots))
	}
}

// a task depending on itself
func TestSelfLoopIsRejected(t *testing.T) {
	def := WorkflowDef{
		Name: "self-loop",
		Tasks: []TaskDef{
			{ID: "a", Handler: "h", DependsOn: []string{"a"}},
		},
	}
	g := NewGraph(def)

	if err := g.Validate(); err == nil {
		t.Error("Validate should reject a self-loop")
	}
	if _, err := g.TopoSort(); err == nil {
		t.Error("TopoSort should reject a self-loop")
	}
}

// Two unconnected clusters. Validate must traverse both, not just the first.
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

// A cycle in a cluster that no root leads to. Catches a DFS that only
// starts from roots: this cycle is unreachable from a1.
func TestDisconnectedCycleIsRejected(t *testing.T) {
	def := WorkflowDef{
		Name: "disconnected-cycle",
		Tasks: []TaskDef{
			{ID: "a1", Handler: "h"},
			{ID: "x", Handler: "h", DependsOn: []string{"y"}},
			{ID: "y", Handler: "h", DependsOn: []string{"x"}},
		},
	}
	g := NewGraph(def)

	if err := g.Validate(); err == nil {
		t.Error("Validate should find a cycle in a disconnected cluster")
	}
}