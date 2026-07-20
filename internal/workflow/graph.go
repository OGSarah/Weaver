// Graph wraps a definition with lookup indexes built once at load time.
// byID gives 0(1) task lookup; children maps a task ID to the IDs it unblocks (the reverse of DependsOn).
type Graph struct {
	Def 		WorkflowDef
	byId		map[string]TaskDef
	children	map[string][]string
}

// NewGraph takes a workflow definition and builds a Graph around it, creating two lookup indexes up front so later operations are fast. 
// It returns a pointer to the Graph (the *Graph in the signature means "pointer to Graph").
func NewGraph(def WorkflowDef) *Graph {
	
	// make() creates and initializes a map that's ready to use. This map's keys
	// are task IDs (string) and its values are TaskDefs, giving us O(1) lookup
	// of a task by its ID instead of scanning a slice every time.
	// The second argument, len(def.Tasks), is a capacity hint: we tell Go roughly
	// how many entries to expect so it can pre-size the map. It's purely an
	// optimization; the map would still work without it.
	byID := make(map[string]TaskDef, len(def.Tasks))
	
	// A second map for the reverse relationship: for a given task ID (the key),
	// the value is the slice of the IDs it unblocks (its downstream tasks).
	// []string means "slice of strings." No capacity hint here since we don't
	// know how many children any task will have
	children := make(map[string][]string)

	// Range over the tasks slice. Range returns two values on each iteration: 
	// the index and the element. We don't need the index, so we assign it to _,
	// the blank identifier, which tells Go "discard this." t is the current TaskDef.
	for _, t:= range def.Tasks {

		// Store this task in the byID map, keyed by its ID. After this loop,
		// byID holds every task, reachable instandly by its ID.
		byID[t.ID] = t
	}

	// A second pass over the same tasks, this time to build the reverse index.
	// We do in in a separate loop (rather than folding it into the one above)
	// mainly for clarity: first fully build byID, then build children.
	for _, t := range def.Tasks {

		// Each task lists the IDs it depends on (its upstreams) in DependsOn.
		// range over that inner slice; upstream is one ID that t depends on.
		for _, upstream := range t.DependsOn {
			// t depends on upstream, so upstream unblocks t.
			// We record that by appending t's ID to upstream's list of children.
			//
			// append(slice, value) returns a (possibly new) slice with value
			// added on the end, and we assign it back to the map entry. This
			// works even if children[upstream] doesn't exist yet: reading a
			// missing map key returns the zero value, which for a slice is nil,
			// and append happily treats nil as an empty slice to add to. That's
			// why we don't need to check "does this key exist" first.
			children[upstream] = append(children[upstream], t.ID)
		}
	}

	// Build a Graph value and return its address with &, so the caller gets a
	// *Graph (a pointer). We store the original def plus the two indexes we just
	// built. The field: value syntax is a struct literal, setting each field by
	// name. Returning a pointer means callers share one Graph rather than copying
	// it, and it matches the *Graph return type declared above.
	return &Graph{Def: def, byID: byID, children: children}
}