package workflow

// TaskDef is the static definition of a single task: what to run and how to run it.
// It deliberately holds no runtime state (no status, no attempt count, no timestamps).
// Those belong to a task's execution inside a specific run, which lands in the database in Phase 2.
type TaskDef struct {
	ID 				string		`json:"id"`
	Handler 		string		`json:"handler"`
	DependsOn		[]string	`json:"dependsOn,omitempty"`
	Retries 		int			`json:"retries,omitempty"`
	TimeoutSeconds	int			`json:"timeoutSeconds,omitempty"` 
}

// WorkflowDef is the static definition of a whole workflow: its metadata plus the set of tasks.
// The dependency edges are expressed by each task listing the IDs it depends on, so the entire workflow is one self-contained document.
type WorkflowDef struct {
	Name 		string		`json:"name"`
	Schedule 	string		`json:"omitempty"`
	Tasks		[]TaskDef	`json:"tasks"`
}