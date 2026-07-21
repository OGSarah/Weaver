// Graph wraps a definition with lookup indexes built once at load time.
// byID gives O(1) task lookup; children maps a task ID to the IDs it unblocks.
type Graph struct {
	Def      WorkflowDef
	byID     map[string]TaskDef
	children map[string][]string
}

// NewGraph builds a Graph from a definition, precomputing lookup indexes.
func NewGraph(def WorkflowDef) *Graph {
	// Task ID -> TaskDef, for O(1) lookup. Second arg pre-sizes the map.
	byID := make(map[string]TaskDef, len(def.Tasks))

	// Task ID -> IDs it unblocks (reverse of DependsOn).
	children := make(map[string][]string)

	// First pass: index every task by its ID.
	for _, t := range def.Tasks {
		byID[t.ID] = t
	}

	// Second pass: build the reverse edges.
	for _, t := range def.Tasks {
		for _, upstream := range t.DependsOn {
			// t depends on upstream, so upstream unblocks t.
			// append handles a missing key: the zero value is a nil slice.
			children[upstream] = append(children[upstream], t.ID)
		}
	}

	return &Graph{Def: def, byID: byID, children: children}
}

// Roots returns the tasks with no upstream dependencies: where a run starts.
func (g *Graph) Roots() []TaskDef {
	// nil is a valid empty slice; append works on it directly.
	var roots []TaskDef

	for _, t := range g.Def.Tasks {
		// A task is a root when its DependsOn is empty. len() covers both
		// nil and empty-but-non-nil slices.
		if len(t.DependsOn) == 0 {
			roots = append(roots, t)
		}
	}

	return roots
}

// Unblocks returns the tasks that become eligible once taskID completes its downstream tasks.
// Takes a task ID and returns full TaskDefs.
func (g *Graph) Unblocks(taskID string) []TaskDef {
	// children[taskID] is the IDs this task unblocks. A missing key returns nil,
	// so a leaf task (unblocks nothing) safely yields no downstream.
	childIDs := g.children[taskID]

	var downstream []TaskDef

	for _, id := range childIDs {
		// Map each downsteam ID back to its full TaskDef via byID.
		downsteam = append(downsteam, g.byID[id])
	}

	return downsteam
}