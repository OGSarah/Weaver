package worker

import (
	"context"
	"fmt"
	"log"

	"weaver/internal/store"
)

// TaskLogger is how a handler records what it is doing. Every line goes to the
// task_logs table against the task and attempt that wrote it, so it survives the
// worker process and shows up in the UI next to the task it belongs to.
//
// It is handed to the handler as an argument rather than pulled out of the context
// because a handler cannot do its job without knowing where to log, and an argument
// says so in the type signature. A context value would make it look optional and
// fail at runtime when a caller forgot to set it.
type TaskLogger struct {
	ctx     context.Context
	store   *store.Store
	taskID  string
	attempt int
	// Name and worker only exist to label the mirrored stdout line below.
	taskName string
	workerID string
}

// Printf records an informational line.
func (l *TaskLogger) Printf(format string, args ...any) {
	l.write(store.LogInfo, fmt.Sprintf(format, args...))
}

// Errorf records a line as an error. It does not fail the task: returning an error
// from the handler is what does that. This is for the detail a returned error is
// too small to carry.
func (l *TaskLogger) Errorf(format string, args ...any) {
	l.write(store.LogError, fmt.Sprintf(format, args...))
}

// write persists one line, and mirrors it to the worker's own stdout so a terminal
// tailing the process still shows what is happening.
//
// A logging failure must never fail the task it is describing, so the error is
// reported to stdout and swallowed. The alternative -- surfacing it to the handler
// -- would mean a database hiccup could fail work that had otherwise succeeded,
// which trades a real guarantee for a cosmetic one.
func (l *TaskLogger) write(level, message string) {
	log.Printf("worker %s: task %s: %s", l.workerID, l.taskName, message)

	// Deliberately not l.ctx: that context carries the task's timeout, and once it
	// expires every further line would be dropped -- including the ones explaining
	// the timeout. The line is small and the write is fast, so it is left to run
	// even as the handler is being torn down.
	if err := l.store.AppendTaskLog(context.WithoutCancel(l.ctx), l.taskID, l.attempt, level, message); err != nil {
		log.Printf("worker %s: recording log line for task %s failed: %v", l.workerID, l.taskName, err)
	}
}
