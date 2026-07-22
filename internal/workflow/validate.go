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