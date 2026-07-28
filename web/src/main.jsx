import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Dag } from "./Dag.jsx";

// The UI is a read layer over the database and nothing more. It picks a workflow and
// draws its DAG; every guarantee that matters (no double execution, retries, dead
// worker recovery) lives in the backend and is invisible from here.
//
// Because the Go API serves this bundle, every fetch below is same-origin: no host,
// no port, no CORS, no dev proxy.

// getJSON wraps fetch with the two things it does not do on its own: treat a 4xx/5xx
// as an error (fetch only rejects on network failure), and decode the body.
async function getJSON(path, signal) {
	const res = await fetch(path, { signal });
	if (!res.ok) {
		throw new Error(`${res.status} ${res.statusText}`);
	}
	return res.json();
}

function App() {
	const [workflows, setWorkflows] = useState([]);
	const [selectedId, setSelectedId] = useState(null);
	const [detail, setDetail] = useState(null);
	const [error, setError] = useState(null);
	const [loading, setLoading] = useState(true);

	// Load the workflow list once. This endpoint returns metadata only: sending every
	// task of every workflow just to populate a menu would not scale.
	useEffect(() => {
		const controller = new AbortController();

		getJSON("/api/workflows", controller.signal)
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

	// Load the selected workflow's definition. This is the request that actually
	// carries the dependency edges, in each task's dependsOn.
	useEffect(() => {
		if (!selectedId) return;

		const controller = new AbortController();
		setDetail(null);

		getJSON(`/api/workflows/${selectedId}`, controller.signal)
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

	if (loading) {
		return <p style={styles.notice}>Loading…</p>;
	}
	if (error) {
		return <p style={{ ...styles.notice, color: "#b91c1c" }}>Error: {error}</p>;
	}

	return (
		<div style={styles.page}>
			<header style={styles.header}>
				<h1 style={styles.title}>Weaver</h1>
			</header>

			<div style={styles.body}>
				<nav style={styles.sidebar}>
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
				</nav>

				<main style={styles.canvas}>
					{detail ? (
						<>
							<div style={styles.canvasHeader}>
								<strong>{detail.name}</strong>
								<span style={styles.wfMeta}>
									{detail.tasks.length} task{detail.tasks.length === 1 ? "" : "s"}
								</span>
							</div>
							<div style={styles.canvasBody}>
								<Dag tasks={detail.tasks} />
							</div>
						</>
					) : (
						<p style={styles.notice}>
							{selectedId ? "Loading graph…" : "Select a workflow."}
						</p>
					)}
				</main>
			</div>
		</div>
	);
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
	// The graph panel needs a real height to scale into, so the body is a flex row
	// that fills what is left of the viewport. minHeight: 0 is the flexbox quirk that
	// lets a flex child actually shrink instead of overflowing its parent.
	body: { display: "flex", flex: 1, minHeight: 0 },
	sidebar: {
		width: 240,
		borderRight: "1px solid #e2e8f0",
		padding: "16px 12px",
		overflowY: "auto",
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
	canvas: { flex: 1, display: "flex", flexDirection: "column", minWidth: 0 },
	canvasHeader: {
		display: "flex",
		alignItems: "baseline",
		gap: 10,
		padding: "12px 20px",
		borderBottom: "1px solid #e2e8f0",
	},
	canvasBody: { flex: 1, minHeight: 0, padding: 20, background: "#f8fafc" },
	notice: { padding: 20, color: "#64748b" },
	hint: { fontSize: 12, color: "#64748b" },
};

// Find the mount point from index.html and hand it to React.
const container = document.getElementById("root");
const root = createRoot(container);
root.render(<App />);
