import React, { useMemo } from "react";
import { layoutDag, edgePath } from "./layout.js";
import { statusColors, STATUS_ORDER } from "./status.js";

// Dag draws a workflow as a directed graph: one box per task, one arrow per
// dependency, roots at the top.
//
// It renders two things from the same component. Given a workflow definition the
// tasks have no status and the boxes show their handler. Given a run's tasks each
// one carries a status, and the box is coloured by it and labelled with it. The
// graph is identical either way, which is the point: a run is just the definition
// with state on top.
//
// Expected task shape: { id, handler, dependsOn?, status?, attempt?, maxAttempts? }
//
// The SVG has no fixed pixel size. It gets a viewBox matching the graph's own
// bounding box, so the browser scales the whole drawing to fit the space available
// and a twenty-task workflow needs no zoom controls to stay on screen.

// Corner rounding on the task boxes.
const NODE_RADIUS = 6;

// Slate for the edges, indigo to mark a root task in the definition view.
const EDGE_COLOR = "#94a3b8";
const ROOT_COLOR = "#6366f1";
const NODE_BORDER = "#cbd5e1";

export function Dag({ tasks, selectedId, onSelect }) {
	// While a run is polling, a new tasks array arrives every second with the same
	// nodes and edges and only the statuses changed. The shape of the graph is what
	// layout is expensive to compute and is exactly what did not change, so the memo
	// keys on the topology alone: ids plus edges, as a string.
	const topologyKey = useMemo(
		() => tasks.map((t) => `${t.id}:${(t.dependsOn ?? []).join(",")}`).join("|"),
		[tasks],
	);

	// eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on topologyKey by design
	const { nodes, edges, width, height } = useMemo(() => layoutDag(tasks), [topologyKey]);

	// The nodes above are only safe to reuse across polls because they are used for
	// geometry alone. They also carry the task objects captured when the layout last
	// ran, which are now stale, so current state is looked up separately by id and
	// the captured copies are never read.
	const currentById = useMemo(() => new Map(tasks.map((t) => [t.id, t])), [tasks]);

	if (nodes.length === 0) {
		return <p style={styles.empty}>This workflow has no tasks.</p>;
	}

	return (
		<div style={styles.wrapper}>
			<svg
				viewBox={`0 0 ${width} ${height}`}
				// meet keeps the aspect ratio and fits the whole graph inside the box,
				// rather than cropping it or stretching the boxes out of shape.
				preserveAspectRatio="xMidYMid meet"
				// The max sizes are the graph's own dimensions, which caps scaling at
				// 1:1. Without them a four-task DAG would be magnified to fill the
				// panel and its labels would render several times their intended size.
				// Large graphs still scale down to fit; small ones just sit centered.
				style={{ ...styles.svg, maxWidth: width, maxHeight: height }}
				role="img"
				aria-label="Workflow dependency graph"
			>
				<defs>
					{/* One arrowhead definition, referenced by every edge. orient="auto"
					    rotates it to match the direction the path arrives from. */}
					<marker
						id="arrowhead"
						viewBox="0 0 10 10"
						refX="9"
						refY="5"
						markerWidth="6"
						markerHeight="6"
						orient="auto"
					>
						<path d="M 0 0 L 10 5 L 0 10 z" fill={EDGE_COLOR} />
					</marker>
				</defs>

				{/* Edges first so the arrows sit behind the boxes: an arrowhead that
				    stops a pixel or two inside a node is hidden rather than drawn on
				    top of it. */}
				<g>
					{edges.map((e) => (
						<path
							key={`${e.from}->${e.to}`}
							d={edgePath(e.points)}
							fill="none"
							stroke={EDGE_COLOR}
							strokeWidth="1.5"
							markerEnd="url(#arrowhead)"
						/>
					))}
				</g>

				<g>
					{nodes.map((n) => (
						<TaskNode
							key={n.id}
							node={n}
							task={currentById.get(n.id)}
							selected={n.id === selectedId}
							onSelect={onSelect}
						/>
					))}
				</g>
			</svg>
		</div>
	);
}

// TaskNode draws one box: its border and tint from the task's status, its labels
// from the definition. node supplies geometry, task supplies current state; they are
// separate because the geometry survives a poll and the state does not.
function TaskNode({ node, task, selected, onSelect }) {
	const { x, y, width, height } = node;
	if (!task) {
		return null;
	}
	const { status } = task;

	// A task with no upstreams is a root: where the scheduler starts a run. It is
	// only worth marking before anything has run, because once a run is live the
	// status colour says something more useful about the same box.
	const isRoot = (task.dependsOn ?? []).length === 0;

	const colors = status ? statusColors(status) : null;
	const fill = colors ? colors.fill : "#ffffff";
	const stroke = colors ? colors.stroke : isRoot ? ROOT_COLOR : NODE_BORDER;
	const strokeWidth = colors ? 2 : isRoot ? 2 : 1.5;

	// In a run the status is the interesting label; the handler is a property of the
	// definition and is one click away in the definition view.
	const sublabel = status ?? task.handler;

	// Retries are the whole point of Phase 5, so surface the attempt count as soon
	// as a task is on its second try. Hidden on the first attempt, which is the
	// common case and would just be noise.
	const showAttempt = (task.attempt ?? 0) > 1;

	return (
		<g
			onClick={onSelect ? () => onSelect(task.id) : undefined}
			style={onSelect ? { cursor: "pointer" } : undefined}
			// Keyboard reachable, because a click target that opens the only view of
			// a task's logs should not be mouse-only.
			tabIndex={onSelect ? 0 : undefined}
			role={onSelect ? "button" : undefined}
			onKeyDown={
				onSelect
					? (e) => {
							if (e.key === "Enter" || e.key === " ") {
								e.preventDefault();
								onSelect(task.id);
							}
						}
					: undefined
			}
		>
			{/* Selection is drawn as a halo behind the node rather than by changing
			    its border, which is already carrying the status colour. */}
			{selected && (
				<rect
					x={x - 4}
					y={y - 4}
					width={width + 8}
					height={height + 8}
					rx={NODE_RADIUS + 3}
					fill="none"
					stroke="#4f46e5"
					strokeWidth="2"
				/>
			)}
			<rect
				x={x}
				y={y}
				width={width}
				height={height}
				rx={NODE_RADIUS}
				fill={fill}
				stroke={stroke}
				strokeWidth={strokeWidth}
				// Cancelled tasks never ran and never will, so the outline is broken to
				// say so without needing another colour.
				strokeDasharray={status === "cancelled" ? "4 3" : undefined}
			>
				{/* A running task is the one thing on screen that is actively changing,
				    so it gets the only motion. SMIL rather than CSS keeps this a
				    self-contained SVG with no stylesheet to bundle. */}
				{status === "running" && (
					<animate
						attributeName="stroke-opacity"
						values="1;0.3;1"
						dur="1.4s"
						repeatCount="indefinite"
					/>
				)}
			</rect>

			{/* Both labels are placed from the box's own coordinates rather than a
			    shared transform, so each can be nudged independently. */}
			<text
				x={x + width / 2}
				y={y + 22}
				textAnchor="middle"
				fontSize="13"
				fontWeight="600"
				fill="#0f172a"
			>
				{task.id}
			</text>
			<text
				x={x + width / 2}
				y={y + 40}
				textAnchor="middle"
				fontSize="11"
				fontWeight={status ? 600 : 400}
				fill={colors ? colors.text : "#64748b"}
			>
				{sublabel}
			</text>

			{showAttempt && (
				<text x={x + width - 8} y={y + 14} textAnchor="end" fontSize="9" fill="#94a3b8">
					try {task.attempt}/{task.maxAttempts}
				</text>
			)}

			{/* A native tooltip on hover. Cheap to add, and the error message is the
			    first thing you want when a node goes red. */}
			{task.error && <title>{task.error}</title>}
		</g>
	);
}

// StatusLegend maps each colour back to its state. Colour alone is not a label:
// without this the difference between orange and red is a guess.
export function StatusLegend() {
	return (
		<ul style={styles.legend}>
			{STATUS_ORDER.map((status) => {
				const c = statusColors(status);
				return (
					<li key={status} style={styles.legendItem}>
						<span
							style={{
								...styles.swatch,
								background: c.fill,
								borderColor: c.stroke,
							}}
						/>
						<span style={{ color: c.text }}>{status}</span>
					</li>
				);
			})}
		</ul>
	);
}

// Styles live inline rather than in a stylesheet on purpose: esbuild is pointed at a
// single JSX entry point and emits one bundle.js, so adding a CSS import would also
// mean an emitted bundle.css and a <link> in index.html. Not worth it for this much
// styling.
const styles = {
	wrapper: {
		width: "100%",
		height: "100%",
		display: "flex",
		alignItems: "center",
		justifyContent: "center",
	},
	svg: {
		width: "100%",
		height: "100%",
		display: "block",
	},
	empty: {
		color: "#64748b",
		fontStyle: "italic",
	},
	legend: {
		display: "flex",
		flexWrap: "wrap",
		gap: "4px 14px",
		listStyle: "none",
		margin: 0,
		padding: "8px 20px",
		fontSize: 11,
		borderTop: "1px solid #e2e8f0",
	},
	legendItem: { display: "flex", alignItems: "center", gap: 5 },
	swatch: {
		width: 11,
		height: 11,
		borderRadius: 3,
		borderWidth: 1.5,
		borderStyle: "solid",
		display: "inline-block",
	},
};
