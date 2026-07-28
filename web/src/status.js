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
//
// Tuned for the dark surfaces in theme.js. The fills are translucent rather than
// opaque so a node picks up the surface beneath it and the whole graph keeps one
// depth, and the text is a light tint of the border colour, because the saturated
// mid-tones that read well as a 1px border are too dim to read as 11px type.
export const STATUS_COLORS = {
	pending: { fill: "rgba(148, 163, 184, 0.08)", stroke: "#475569", text: "#93a1b5" },
	ready: { fill: "rgba(59, 130, 246, 0.14)", stroke: "#3b82f6", text: "#93c5fd" },
	running: { fill: "rgba(245, 158, 11, 0.16)", stroke: "#f59e0b", text: "#fcd34d" },
	succeeded: { fill: "rgba(34, 197, 94, 0.14)", stroke: "#22c55e", text: "#86efac" },
	failed: { fill: "rgba(251, 146, 60, 0.16)", stroke: "#fb923c", text: "#fdba74" },
	dead: { fill: "rgba(239, 68, 68, 0.16)", stroke: "#ef4444", text: "#fca5a5" },
	cancelled: { fill: "rgba(100, 116, 139, 0.08)", stroke: "#64748b", text: "#8b98ab" },
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
const UNKNOWN = { fill: "rgba(148, 163, 184, 0.08)", stroke: "#475569", text: "#93a1b5" };

export function statusColors(status) {
	return STATUS_COLORS[status] ?? UNKNOWN;
}

// breakdown turns a {status: count} map into ordered [status, count] pairs, using
// the lifecycle order above and dropping statuses nothing is in.
//
// Any status the palette does not know about is kept and appended, so a state added
// to the schema before it is added here still shows up in a total that adds up,
// rather than silently going missing from a "2 of 4" that then looks wrong.
export function breakdown(counts) {
	if (!counts) return [];
	const known = STATUS_ORDER.filter((s) => counts[s] > 0).map((s) => [s, counts[s]]);
	const extra = Object.entries(counts)
		.filter(([s, n]) => n > 0 && !STATUS_COLORS[s])
		.sort(([a], [b]) => a.localeCompare(b));
	return [...known, ...extra];
}

// countByStatus builds the same map from a run's task list, so the run view can
// summarize itself without a second request shaped like the history endpoint's.
export function countByStatus(tasks) {
	const counts = {};
	for (const t of tasks) {
		counts[t.status] = (counts[t.status] ?? 0) + 1;
	}
	return counts;
}

// Run-level statuses that mean nothing further will change, so polling can stop.
// Anything else (pending, running) is still in flight.
const TERMINAL_RUN_STATUSES = new Set(["succeeded", "failed", "cancelled"]);

export function isRunFinished(status) {
	return TERMINAL_RUN_STATUSES.has(status);
}
