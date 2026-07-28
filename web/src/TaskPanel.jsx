import React from "react";
import { statusColors } from "./status.js";

// TaskPanel is everything known about one task: its state, its timings, why it
// failed if it did, whatever the handler returned, and its log.
//
// It is a pure render of what the caller passes in. Fetching and polling belong to
// the caller, because the panel has to stay in step with the run view beside it and
// two components polling the same task at different moments would disagree.

export function TaskPanel({ task, loading, onClose }) {
	return (
		<aside style={styles.panel}>
			<header style={styles.header}>
				<strong style={styles.title}>{task ? task.name : "Task"}</strong>
				<button onClick={onClose} style={styles.close} aria-label="Close task panel">
					×
				</button>
			</header>

			{!task ? (
				<p style={styles.muted}>{loading ? "Loading…" : "No task selected."}</p>
			) : (
				<div style={styles.body}>
					<Facts task={task} />
					{task.error && (
						<section>
							<h3 style={styles.sectionTitle}>Error</h3>
							<pre style={styles.error}>{task.error}</pre>
						</section>
					)}
					{task.result && (
						<section>
							<h3 style={styles.sectionTitle}>Result</h3>
							<pre style={styles.code}>{JSON.stringify(task.result, null, 2)}</pre>
						</section>
					)}
					<LogView logs={task.logs} truncated={task.logsTruncated} />
				</div>
			)}
		</aside>
	);
}

// Facts is the flat key/value part: state, retry budget, and timings.
function Facts({ task }) {
	const c = statusColors(task.status);
	return (
		<dl style={styles.facts}>
			<Fact label="Status">
				<span
					style={{ ...styles.pill, background: c.fill, borderColor: c.stroke, color: c.text }}
				>
					{task.status}
				</span>
			</Fact>
			<Fact label="Handler">
				<code>{task.handler}</code>
			</Fact>
			<Fact label="Attempt">
				{task.attempt} of {task.maxAttempts}
			</Fact>
			<Fact label="Timeout">{task.timeoutSeconds}s</Fact>
			<Fact label="Scheduled">{formatTime(task.scheduledAt)}</Fact>
			<Fact label="Started">{formatTime(task.startedAt)}</Fact>
			<Fact label="Finished">{formatTime(task.finishedAt)}</Fact>
			<Fact label="Duration">{duration(task.startedAt, task.finishedAt)}</Fact>
		</dl>
	);
}

function Fact({ label, children }) {
	return (
		<>
			<dt style={styles.factLabel}>{label}</dt>
			<dd style={styles.factValue}>{children}</dd>
		</>
	);
}

// LogView renders the log grouped by attempt. The grouping is the point: on a
// retried task a flat list buries the boundary between "this failed" and "this is
// the run that worked", which is the thing you opened the panel to find.
function LogView({ logs, truncated }) {
	if (!logs || logs.length === 0) {
		return (
			<section>
				<h3 style={styles.sectionTitle}>Log</h3>
				<p style={styles.muted}>No log lines yet.</p>
			</section>
		);
	}

	// Lines arrive oldest first, so walking once and starting a new group whenever
	// the attempt changes keeps them in order without a sort.
	const groups = [];
	for (const line of logs) {
		const last = groups[groups.length - 1];
		if (!last || last.attempt !== line.attempt) {
			groups.push({ attempt: line.attempt, lines: [line] });
		} else {
			last.lines.push(line);
		}
	}

	return (
		<section>
			<h3 style={styles.sectionTitle}>Log</h3>
			{truncated && (
				<p style={styles.truncated}>
					Older lines omitted; showing the most recent only.
				</p>
			)}
			{groups.map((g) => (
				<div key={g.attempt} style={styles.attemptGroup}>
					<div style={styles.attemptLabel}>attempt {g.attempt}</div>
					<ol style={styles.log}>
						{g.lines.map((line, i) => (
							<li key={i} style={styles.logLine}>
								<span style={styles.logTime}>{formatClock(line.loggedAt)}</span>
								<span style={line.level === "error" ? styles.logError : undefined}>
									{line.message}
								</span>
							</li>
						))}
					</ol>
				</div>
			))}
		</section>
	);
}

// The API sends RFC3339 in UTC; show local time, since the reader is local.
function formatTime(iso) {
	if (!iso) return <span style={styles.muted}>—</span>;
	return new Date(iso).toLocaleString();
}

function formatClock(iso) {
	if (!iso) return "";
	const d = new Date(iso);
	return d.toLocaleTimeString(undefined, { hour12: false }) + "." +
		String(d.getMilliseconds()).padStart(3, "0");
}

// duration reports how long an attempt took, or how long the current one has been
// running. A task that is still going has no finished_at, and "running for 4s" is
// more useful there than an em dash.
function duration(startedAt, finishedAt) {
	if (!startedAt) return <span style={styles.muted}>—</span>;
	const start = new Date(startedAt).getTime();
	const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
	const ms = end - start;
	const text = ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`;
	return finishedAt ? text : `${text} (running)`;
}

const styles = {
	panel: {
		width: 360,
		borderLeft: "1px solid #e2e8f0",
		display: "flex",
		flexDirection: "column",
		minHeight: 0,
		background: "#ffffff",
	},
	header: {
		display: "flex",
		alignItems: "center",
		gap: 8,
		padding: "10px 14px",
		borderBottom: "1px solid #e2e8f0",
	},
	title: { flex: 1, fontSize: 14 },
	close: {
		border: "none",
		background: "none",
		fontSize: 20,
		lineHeight: 1,
		cursor: "pointer",
		color: "#64748b",
		padding: 0,
	},
	// The panel scrolls on its own; a long log must not stretch the page.
	body: { overflowY: "auto", padding: "12px 14px", fontSize: 13 },
	facts: {
		display: "grid",
		gridTemplateColumns: "auto 1fr",
		gap: "4px 12px",
		margin: "0 0 14px",
		alignItems: "baseline",
	},
	factLabel: { color: "#64748b", fontSize: 12 },
	factValue: { margin: 0, wordBreak: "break-word" },
	pill: {
		fontSize: 11,
		fontWeight: 600,
		padding: "1px 8px",
		borderRadius: 999,
		borderWidth: 1,
		borderStyle: "solid",
	},
	sectionTitle: {
		fontSize: 11,
		textTransform: "uppercase",
		letterSpacing: "0.06em",
		color: "#64748b",
		margin: "0 0 6px",
	},
	error: {
		margin: "0 0 14px",
		padding: 8,
		background: "#fef2f2",
		color: "#b91c1c",
		borderRadius: 4,
		fontSize: 12,
		whiteSpace: "pre-wrap",
		wordBreak: "break-word",
	},
	code: {
		margin: "0 0 14px",
		padding: 8,
		background: "#f8fafc",
		borderRadius: 4,
		fontSize: 12,
		overflowX: "auto",
	},
	attemptGroup: { marginBottom: 10 },
	attemptLabel: {
		fontSize: 11,
		fontWeight: 600,
		color: "#475569",
		borderTop: "1px solid #e2e8f0",
		paddingTop: 4,
		marginBottom: 2,
	},
	log: { listStyle: "none", margin: 0, padding: 0 },
	logLine: {
		display: "flex",
		gap: 8,
		fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
		fontSize: 11,
		lineHeight: 1.6,
		wordBreak: "break-word",
	},
	logTime: { color: "#94a3b8", flexShrink: 0 },
	logError: { color: "#b91c1c" },
	truncated: { fontSize: 11, color: "#b45309", margin: "0 0 6px" },
	muted: { color: "#64748b", padding: "0 0 8px" },
};
