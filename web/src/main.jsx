import React, { useCallback, useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import { Dag, StatusLegend } from "./Dag.jsx";
import { TaskPanel } from "./TaskPanel.jsx";
import { breakdown, countByStatus, isRunFinished, statusColors } from "./status.js";

// The UI is a read layer over the database and nothing more. It picks a workflow,
// draws its DAG, triggers runs, watches them, and reads back what each task did.
// Every guarantee that matters (no double execution, retries, dead worker recovery)
// lives in the backend and is invisible from here. A task turning from amber to
// green in this page is the worker loop and the store finishing a transaction,
// nothing else.
//
// Because the Go API serves this bundle, every fetch below is same-origin: no host,
// no port, no CORS, no dev proxy.

// How often to re-fetch a live run. A second is comfortably below the pace a human
// notices as lag, and the query behind it is a few indexed reads on one run.
const POLL_MS = 1000;

// getJSON wraps fetch with the two things it does not do on its own: treat a 4xx/5xx
// as an error (fetch only rejects on network failure), and decode the body.
async function getJSON(path, { signal, method = "GET" } = {}) {
	const res = await fetch(path, { method, signal });
	if (!res.ok) {
		// Error responses share one shape, so try for the server's message before
		// falling back to the status line.
		let detail = `${res.status} ${res.statusText}`;
		try {
			const body = await res.json();
			if (body?.error) detail = body.error;
		} catch {
			// Body was not JSON; the status line will do.
		}
		throw new Error(detail);
	}
	return res.json();
}

function App() {
	const [workflows, setWorkflows] = useState([]);
	const [selectedId, setSelectedId] = useState(null);
	const [detail, setDetail] = useState(null);
	const [history, setHistory] = useState([]);
	// Bumped whenever something should have changed the history list. Cheaper than
	// refetching it on every poll, and more reliable than hoping it is still fresh.
	const [historyNonce, setHistoryNonce] = useState(0);
	const [runId, setRunId] = useState(null);
	const [run, setRun] = useState(null);
	const [selectedTaskName, setSelectedTaskName] = useState(null);
	const [taskDetail, setTaskDetail] = useState(null);
	const [error, setError] = useState(null);
	const [loading, setLoading] = useState(true);
	const [triggering, setTriggering] = useState(false);

	// Load the workflow list once. This endpoint returns metadata only: sending every
	// task of every workflow just to populate a menu would not scale.
	useEffect(() => {
		const controller = new AbortController();

		getJSON("/api/workflows", { signal: controller.signal })
			.then((data) => {
				setWorkflows(data);
				// Pick the first one so the page shows a graph immediately rather than
				// an empty panel with a prompt.
				if (data.length > 0) {
					setSelectedId(data[0].id);
				}
				setLoading(false);
			})
			.catch((err) => {
				// An abort is the component unmounting, not a failure.
				if (err.name === "AbortError") return;
				setError(err.message);
				setLoading(false);
			});

		// Abort on unmount so a slow response cannot set state on a gone component.
		return () => controller.abort();
	}, []);

	// Load the selected workflow's definition. This is the request that carries the
	// dependency edges, in each task's dependsOn.
	useEffect(() => {
		if (!selectedId) return;

		const controller = new AbortController();
		setDetail(null);
		// A run belongs to the workflow that was showing when it was opened, so
		// changing workflows drops it rather than leaving a graph from elsewhere.
		setRunId(null);
		setRun(null);
		setSelectedTaskName(null);

		getJSON(`/api/workflows/${selectedId}`, { signal: controller.signal })
			.then(setDetail)
			.catch((err) => {
				if (err.name === "AbortError") return;
				setError(err.message);
			});

		// Aborting here also covers the user clicking a second workflow before the
		// first response lands: without it, a slow earlier request could resolve last
		// and draw the wrong graph.
		return () => controller.abort();
	}, [selectedId]);

	// Load this workflow's run history.
	useEffect(() => {
		if (!selectedId) return;

		const controller = new AbortController();
		getJSON(`/api/workflows/${selectedId}/runs`, { signal: controller.signal })
			.then(setHistory)
			.catch((err) => {
				if (err.name === "AbortError") return;
				setError(err.message);
			});

		return () => controller.abort();
	}, [selectedId, historyNonce]);

	// Poll the open run until it reaches a terminal state.
	//
	// This chains setTimeout off each completed response rather than using
	// setInterval. An interval fires on a fixed clock whether or not the previous
	// request came back, so a slow response gets a second one stacked behind it and
	// they can land out of order, flickering the graph backwards. Chaining means
	// there is never more than one request in flight.
	useEffect(() => {
		if (!runId) return;

		const controller = new AbortController();
		let stopped = false;
		let timer;

		async function poll() {
			try {
				const data = await getJSON(`/api/runs/${runId}`, { signal: controller.signal });
				if (stopped) return;
				setRun(data);
				if (!isRunFinished(data.status)) {
					timer = setTimeout(poll, POLL_MS);
				} else {
					// The run just reached its final state, so its history row is now
					// out of date. One refresh, not one per poll.
					setHistoryNonce((n) => n + 1);
				}
			} catch (err) {
				if (stopped || err.name === "AbortError") return;
				setError(err.message);
			}
		}
		poll();

		// stopped guards the async gap: an in-flight response can still resolve after
		// the abort, and without the flag it would set state for a run we left.
		return () => {
			stopped = true;
			controller.abort();
			clearTimeout(timer);
		};
	}, [runId]);

	// The run's tasks in the shape Dag expects. A run identifies a task by name (its
	// row id is regenerated per run), which is what the definition calls id, so the
	// two views feed the same component.
	const runTasks = useMemo(() => {
		if (!run) return null;
		return run.tasks.map((t) => ({
			id: t.name,
			handler: t.handler,
			dependsOn: t.dependsOn,
			status: t.status,
			attempt: t.attempt,
			maxAttempts: t.maxAttempts,
			error: t.error,
		}));
	}, [run]);

	// The graph is keyed by task name; the detail endpoint wants the row's uuid.
	// Resolving here keeps Dag ignorant of the difference.
	const selectedTaskId = useMemo(() => {
		if (!run || !selectedTaskName) return null;
		return run.tasks.find((t) => t.name === selectedTaskName)?.id ?? null;
	}, [run, selectedTaskName]);

	// Load the selected task's detail and log. This deliberately re-runs whenever
	// `run` changes, which is once per poll, so an open panel tails a running task's
	// log at the same cadence as the graph. When the run is finished the poll stops,
	// `run` stops changing, and this stops with it.
	useEffect(() => {
		if (!runId || !selectedTaskId) {
			setTaskDetail(null);
			return;
		}

		const controller = new AbortController();
		getJSON(`/api/runs/${runId}/tasks/${selectedTaskId}`, { signal: controller.signal })
			.then(setTaskDetail)
			.catch((err) => {
				if (err.name === "AbortError") return;
				setError(err.message);
			});

		return () => controller.abort();
	}, [runId, selectedTaskId, run]);

	const triggerRun = useCallback(async () => {
		if (!selectedId) return;
		setTriggering(true);
		setError(null);
		try {
			const { runId: id } = await getJSON(`/api/workflows/${selectedId}/runs`, {
				method: "POST",
			});
			// Clearing the previous run first means the panel shows "waiting" rather
			// than the old run's colours until the first poll lands.
			setRun(null);
			setSelectedTaskName(null);
			setRunId(id);
			setHistoryNonce((n) => n + 1);
		} catch (err) {
			setError(err.message);
		} finally {
			setTriggering(false);
		}
	}, [selectedId]);

	const openRun = useCallback((id) => {
		setRun(null);
		setSelectedTaskName(null);
		setRunId(id);
	}, []);

	if (loading) {
		return <p style={styles.notice}>Loading…</p>;
	}

	const showingRun = Boolean(runId);
	const tasks = showingRun ? runTasks : detail?.tasks;

	return (
		<div style={styles.page}>
			<header style={styles.header}>
				<h1 style={styles.title}>Weaver</h1>
			</header>

			{error && (
				<div style={styles.errorBar}>
					{error}
					<button onClick={() => setError(null)} style={styles.dismiss}>
						dismiss
					</button>
				</div>
			)}

			<div style={styles.body}>
				{/* Two independently scrolling sections rather than one long column.
				    The workflow list has no upper bound -- a database that has been
				    used for a while has hundreds -- and if the history simply followed
				    it, the history would be somewhere off the bottom of the page. */}
				<nav style={styles.sidebar}>
					<section style={styles.sidebarScroll}>
						<h2 style={styles.sidebarTitle}>Workflows</h2>
						{workflows.length === 0 && (
							<p style={styles.hint}>
								No workflows registered. POST one to <code>/api/workflows</code>.
							</p>
						)}
						<ul style={styles.list}>
							{workflows.map((wf) => (
								<li key={wf.id}>
									<button
										onClick={() => setSelectedId(wf.id)}
										style={{
											...styles.listButton,
											...(wf.id === selectedId ? styles.listButtonActive : null),
										}}
									>
										<span style={styles.wfName}>{wf.name}</span>
										<span style={styles.wfMeta}>
											v{wf.version}
											{wf.schedule ? ` · ${wf.schedule}` : ""}
										</span>
									</button>
								</li>
							))}
						</ul>
					</section>

					<section style={styles.sidebarHistory}>
						<h2 style={styles.sidebarTitle}>Run history</h2>
						<RunHistory
							runs={history}
							openRunId={runId}
							onOpen={openRun}
							latestVersion={detail?.version}
							// The history list is only refetched when a run starts or
							// finishes, so the open run's own row would sit stale for
							// the whole time it is running, contradicting the header
							// right next to it. Hand the row the live state instead.
							openRunLive={
								run ? { status: run.status, counts: countByStatus(run.tasks) } : null
							}
						/>
					</section>
				</nav>

				<main style={styles.canvas}>
					{detail ? (
						<>
							<div style={styles.canvasHeader}>
								<strong>{detail.name}</strong>
								{showingRun ? (
									<RunSummary run={run} />
								) : (
									<span style={styles.wfMeta}>
										{detail.tasks.length} task
										{detail.tasks.length === 1 ? "" : "s"}
									</span>
								)}
								<span style={styles.spacer} />
								{showingRun && (
									<button
										onClick={() => {
											setRunId(null);
											setRun(null);
											setSelectedTaskName(null);
										}}
										style={styles.secondaryButton}
									>
										Back to definition
									</button>
								)}
								<button
									onClick={triggerRun}
									disabled={triggering}
									style={styles.primaryButton}
								>
									{triggering ? "Triggering…" : "Trigger run"}
								</button>
							</div>

							<div style={styles.canvasBody}>
								{tasks ? (
									<Dag
										tasks={tasks}
										selectedId={showingRun ? selectedTaskName : null}
										// Selection only means something once there is
										// state to show, so the definition view's nodes
										// are not clickable.
										onSelect={showingRun ? setSelectedTaskName : undefined}
									/>
								) : (
									<p style={styles.notice}>Waiting for the run to appear…</p>
								)}
							</div>

							{/* The legend is only meaningful once something is coloured. */}
							{showingRun && <StatusLegend />}
						</>
					) : (
						<p style={styles.notice}>
							{selectedId ? "Loading graph…" : "Select a workflow."}
						</p>
					)}
				</main>

				{showingRun && selectedTaskName && (
					<TaskPanel
						task={taskDetail}
						loading={!taskDetail}
						onClose={() => setSelectedTaskName(null)}
					/>
				)}
			</div>
		</div>
	);
}

// RunHistory lists a workflow's recent runs, newest first.
function RunHistory({ runs, openRunId, onOpen, latestVersion, openRunLive }) {
	if (runs.length === 0) {
		return <p style={styles.hint}>No runs yet.</p>;
	}
	return (
		<ul style={styles.list}>
			{runs.map((r) => {
				// Live state for the run being watched, the stored row for the rest.
				const live = r.id === openRunId ? openRunLive : null;
				const status = live?.status ?? r.status;
				const counts = live?.counts ?? r.taskCounts;
				const c = statusColors(status);
				const parts = breakdown(counts);
				const succeeded = counts?.succeeded ?? 0;
				return (
					<li key={r.id}>
						<button
							onClick={() => onOpen(r.id)}
							style={{
								...styles.listButton,
								...(r.id === openRunId ? styles.listButtonActive : null),
							}}
							// The bar is the at-a-glance version; this is the exact one,
							// for anyone who wants the numbers rather than the shape.
							title={parts.map(([s, n]) => `${n} ${s}`).join(", ")}
						>
							<span style={styles.runRow}>
								<span
									style={{
										...styles.dot,
										background: c.fill,
										borderColor: c.stroke,
									}}
								/>
								<span style={{ color: c.text, fontWeight: 600, fontSize: 12 }}>
									{status}
								</span>
								<span style={styles.wfMeta}>
									{succeeded}/{r.taskCount}
								</span>
							</span>

							<TaskBar parts={parts} total={r.taskCount} />

							<span style={styles.wfMeta}>
								{relativeTime(r.createdAt)}
								{/* Only worth mentioning a version when it is not the
								    one the graph above is currently drawn from. */}
								{latestVersion !== undefined && r.workflowVersion !== latestVersion
									? ` · v${r.workflowVersion}`
									: ""}
							</span>
						</button>
					</li>
				);
			})}
		</ul>
	);
}

// TaskBar is a stacked bar of a run's task states, one segment per status, sized by
// share of the total. In a 240px column this says "mostly green with one red bit"
// faster than any arrangement of numbers, and it is the whole run rather than the
// single number a "2/4" reduces it to.
function TaskBar({ parts, total }) {
	if (!total || parts.length === 0) return null;
	return (
		<span style={styles.bar} aria-hidden="true">
			{parts.map(([status, n]) => {
				const c = statusColors(status);
				return (
					<span
						key={status}
						style={{
							width: `${(n / total) * 100}%`,
							background: c.stroke,
							display: "block",
						}}
					/>
				);
			})}
		</span>
	);
}

// RunSummary is the one-line state of the open run: its own status, how many tasks
// have finished, and whether the page is still polling.
function RunSummary({ run }) {
	if (!run) {
		return <span style={styles.wfMeta}>starting…</span>;
	}
	const live = !isRunFinished(run.status);
	const c = statusColors(run.status);
	// The header has room for words, so it spells the breakdown out instead of
	// drawing it. Same data as the sidebar bar, same ordering.
	const parts = breakdown(countByStatus(run.tasks));

	return (
		<span style={styles.runSummary}>
			<span style={{ ...styles.pill, background: c.fill, borderColor: c.stroke, color: c.text }}>
				{run.status}
			</span>
			<span style={styles.counts}>
				{parts.map(([status, n]) => (
					<span key={status} style={{ color: statusColors(status).text }}>
						{n} {status}
					</span>
				))}
			</span>
			{/* Say plainly whether the numbers are still moving. A stopped poll and a
			    stalled run look identical otherwise. */}
			<span style={styles.wfMeta}>{live ? "· polling" : "· finished"}</span>
		</span>
	);
}

// relativeTime keeps the history list scannable: exact timestamps are all the same
// width and shape, so "2m ago" is easier to compare down a column.
function relativeTime(iso) {
	if (!iso) return "";
	const seconds = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
	if (seconds < 60) return `${Math.max(seconds, 0)}s ago`;
	if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
	if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
	return `${Math.floor(seconds / 86400)}d ago`;
}

const styles = {
	page: {
		font: "14px/1.5 system-ui, -apple-system, sans-serif",
		color: "#0f172a",
		height: "100vh",
		display: "flex",
		flexDirection: "column",
	},
	header: {
		padding: "12px 20px",
		borderBottom: "1px solid #e2e8f0",
	},
	title: { margin: 0, fontSize: 18 },
	errorBar: {
		display: "flex",
		alignItems: "center",
		gap: 10,
		padding: "8px 20px",
		background: "#fef2f2",
		color: "#b91c1c",
		borderBottom: "1px solid #fecaca",
		fontSize: 13,
	},
	dismiss: {
		border: "none",
		background: "none",
		color: "#b91c1c",
		textDecoration: "underline",
		cursor: "pointer",
		font: "inherit",
	},
	// The graph panel needs a real height to scale into, so the body is a flex row
	// that fills what is left of the viewport. minHeight: 0 is the flexbox quirk that
	// lets a flex child actually shrink instead of overflowing its parent.
	body: { display: "flex", flex: 1, minHeight: 0 },
	sidebar: {
		width: 240,
		flexShrink: 0,
		borderRight: "1px solid #e2e8f0",
		display: "flex",
		flexDirection: "column",
		minHeight: 0,
	},
	// Takes the leftover height and scrolls inside it.
	sidebarScroll: { flex: 1, minHeight: 0, overflowY: "auto", padding: "16px 12px" },
	// Capped so a long history cannot squeeze the workflow list out, and so the
	// section is always on screen no matter how many workflows exist.
	sidebarHistory: {
		flexShrink: 0,
		maxHeight: "45%",
		overflowY: "auto",
		padding: "12px",
		borderTop: "1px solid #e2e8f0",
	},
	sidebarTitle: {
		margin: "0 0 8px",
		fontSize: 11,
		textTransform: "uppercase",
		letterSpacing: "0.06em",
		color: "#64748b",
	},
	list: { listStyle: "none", margin: 0, padding: 0 },
	listButton: {
		display: "flex",
		flexDirection: "column",
		alignItems: "flex-start",
		gap: 2,
		width: "100%",
		padding: "8px 10px",
		marginBottom: 2,
		border: "1px solid transparent",
		borderRadius: 6,
		background: "none",
		font: "inherit",
		textAlign: "left",
		cursor: "pointer",
	},
	// Repeats the whole border rather than overriding borderColor alone: React warns
	// when a shorthand and a longhand for the same property are mixed across renders,
	// because which one wins depends on render order.
	listButtonActive: { background: "#eef2ff", border: "1px solid #c7d2fe" },
	wfName: { fontWeight: 600 },
	wfMeta: { fontSize: 12, color: "#64748b" },
	runRow: { display: "flex", alignItems: "center", gap: 6 },
	dot: {
		width: 9,
		height: 9,
		borderRadius: "50%",
		borderWidth: 1.5,
		borderStyle: "solid",
		display: "inline-block",
		flexShrink: 0,
	},
	canvas: { flex: 1, display: "flex", flexDirection: "column", minWidth: 0 },
	canvasHeader: {
		display: "flex",
		alignItems: "center",
		gap: 10,
		padding: "10px 20px",
		borderBottom: "1px solid #e2e8f0",
	},
	spacer: { flex: 1 },
	canvasBody: { flex: 1, minHeight: 0, padding: 20, background: "#f8fafc" },
	runSummary: { display: "flex", alignItems: "center", gap: 8 },
	// Wraps rather than overflowing: a run mid-flight can be in four states at once,
	// and the header must not push the buttons off the right edge.
	counts: { display: "flex", flexWrap: "wrap", gap: "0 10px", fontSize: 12 },
	bar: {
		display: "flex",
		width: "100%",
		height: 4,
		borderRadius: 2,
		overflow: "hidden",
		margin: "3px 0 1px",
		background: "#e2e8f0",
	},
	pill: {
		fontSize: 11,
		fontWeight: 600,
		padding: "1px 8px",
		borderRadius: 999,
		borderWidth: 1,
		borderStyle: "solid",
	},
	primaryButton: {
		padding: "6px 12px",
		borderRadius: 6,
		border: "1px solid #4f46e5",
		background: "#4f46e5",
		color: "#ffffff",
		font: "inherit",
		fontSize: 13,
		cursor: "pointer",
	},
	secondaryButton: {
		padding: "6px 12px",
		borderRadius: 6,
		border: "1px solid #cbd5e1",
		background: "#ffffff",
		color: "#0f172a",
		font: "inherit",
		fontSize: 13,
		cursor: "pointer",
	},
	notice: { padding: 20, color: "#64748b" },
	hint: { fontSize: 12, color: "#64748b" },
};

// Find the mount point from index.html and hand it to React.
const container = document.getElementById("root");
const root = createRoot(container);
root.render(<App />);
