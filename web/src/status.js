// The task state palette, in one place because the graph and the legend must never
// drift apart.
//
// The states are the ones the tasks_status_valid constraint allows, and the colours
// encode the distinction that matters most when you are watching a run: whether a
// task is still going to do something.
//
//   pending    waiting on an upstream task            grey, inert
//   ready      unblocked, waiting for a free worker   blue, queued
//   running    a worker holds a lease on it           amber, in flight
//   succeeded  done                                   green
//   failed     the attempt failed, a retry is due     orange, still hope
//   dead       attempts exhausted, gives up           red, terminal
//   cancelled  the run was cancelled before it ran    grey, dashed
//
// failed and dead are deliberately different hues rather than two shades of red.
// "will try again" and "has given up" are the single most important thing to be
// able to tell apart at a glance, and they are one attempt apart in the data.
export const STATUS_COLORS = {
	pending: { fill: "#f8fafc", stroke: "#cbd5e1", text: "#64748b" },
	ready: { fill: "#eff6ff", stroke: "#3b82f6", text: "#1d4ed8" },
	running: { fill: "#fffbeb", stroke: "#f59e0b", text: "#b45309" },
	succeeded: { fill: "#f0fdf4", stroke: "#22c55e", text: "#15803d" },
	failed: { fill: "#fff7ed", stroke: "#fb923c", text: "#c2410c" },
	dead: { fill: "#fef2f2", stroke: "#dc2626", text: "#b91c1c" },
	cancelled: { fill: "#f8fafc", stroke: "#94a3b8", text: "#64748b" },
};

// Legend order: roughly the order a task passes through them.
export const STATUS_ORDER = [
	"pending",
	"ready",
	"running",
	"succeeded",
	"failed",
	"dead",
	"cancelled",
];

// Fallback for a status the UI does not know about, so a future state added to the
// schema renders as a plain node rather than crashing on an undefined lookup.
const UNKNOWN = { fill: "#ffffff", stroke: "#cbd5e1", text: "#64748b" };

export function statusColors(status) {
	return STATUS_COLORS[status] ?? UNKNOWN;
}

// Run-level statuses that mean nothing further will change, so polling can stop.
// Anything else (pending, running) is still in flight.
const TERMINAL_RUN_STATUSES = new Set(["succeeded", "failed", "cancelled"]);

export function isRunFinished(status) {
	return TERMINAL_RUN_STATUSES.has(status);
}
