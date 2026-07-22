import "fmt"

// Node states during the depth-first traversal.
const (
	unvisited = iota // never seen
	visiting         // on the current DFS path (its descendants are still being explored)
	done             // fully explored, no cycle found through it
)

// Validate reports an error if the workflow contains a cycle, using a
// depth-first search. A cycle exists if the DFS ever reaches a node that is
// still "visiting", meaning we looped back onto our own current path.
func (g *Graph) Validate() error {
	// Tracks each task's state across the whole traversal.
	state := make(map[string]int, len(g.Def.Tasks))

	// Start a DFS from every task so disconnected pieces are all covered.
	for _, t := range g.Def.Tasks {
		if state[t.ID] == unvisited {
			if err := g.visit(t.ID, state); err != nil {
				return err
			}
		}
	}
	return nil
}

// visit explores taskID and everything downstream of it. state is shared
// across the whole traversal so we don't re-explore finished nodes.
func (g *Graph) visit(taskID string, state map[string]int) error {
	// Mark this node as on the current path before exploring its children.
	state[taskID] = visiting

	for _, child := range g.children[taskID] {
		switch state[child] {
		case visiting:
			// The child is already on our current path: we've looped back,
			// so there is a cycle.
			return fmt.Errorf("cycle detected involving task %q", child)
		case unvisited:
			// Explore deeper; bubble up any cycle found below.
			if err := g.visit(child, state); err != nil {
				return err
			}
			// case done: already fully explored, safe to skip.
		}
	}

	// All descendants explored without finding a cycle through this node.
	state[taskID] = done
	return nil
}

// TopoSort returns the tasks in a valid execution order: every task appears
// after all of its upstream dependencies. Returns an error if the graph is
// cyclic, since no such ordering exists.
func (g *Graph) TopoSort() ([]TaskDef, error) {
	// Count how many upstreams each task is still waiting on.
	remaining := make(map[string]int, len(g.Def.Tasks))
	for _, t := range g.Def.Tasks {
		remaining[t.ID] = len(t.DependsOn)
	}

	// Seed the queue with the roots: tasks waiting on nothing.
	var queue []string
	for _, t := range g.Def.Tasks {
		if remaining[t.ID] == 0 {
			queue = append(queue, t.ID)
		}
	}

	var order []TaskDef
	for len(queue) > 0 {
		// Pop the front of the queue.
		id := queue[0]
		queue = queue[1:]

		order = append(order, g.byID[id])

		// This task is done, so each downstream task waits on one fewer.
		for _, child := range g.children[id] {
			remaining[child]--
			// Once nothing blocks it, it is ready to be ordered.
			if remaining[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	// Any task never ordered is stuck waiting on something that never
	// completed, which only happens inside a cycle.
	if len(order) != len(g.Def.Tasks) {
		return nil, fmt.Errorf("cycle detected: only ordered %d of %d tasks", len(order), len(g.Def.Tasks))
	}

	return order, nil
}