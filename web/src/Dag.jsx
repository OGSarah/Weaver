import React, { useMemo } from "react";
import { layoutDag, edgePath } from "./layout.js";

// Dag draws a workflow definition as a directed graph: one box per task, one arrow
// per dependency, roots at the top.
//
// The SVG has no fixed pixel size. It gets a viewBox matching the graph's own
// bounding box, so the browser scales the whole drawing to fit the space available
// and a twenty-task workflow needs no zoom controls to stay on screen.

// Corner rounding on the task boxes.
const NODE_RADIUS = 6;

// Slate for the edges, indigo to mark a root task.
const EDGE_COLOR = "#94a3b8";
const ROOT_COLOR = "#6366f1";
const NODE_BORDER = "#cbd5e1";

export function Dag({ tasks }) {
	// Layout is pure and only depends on the task list, so it is memoized: React
	// re-renders for all sorts of reasons and running a graph layout on each one
	// would be wasteful.
	const { nodes, edges, width, height } = useMemo(() => layoutDag(tasks), [tasks]);

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
					{nodes.map((n) => {
						// A task with no upstreams is a root: where the scheduler starts
						// a run. Worth marking, because it is the one piece of runtime
						// behaviour you can read off the graph before anything executes.
						const isRoot = (n.task.dependsOn ?? []).length === 0;
						return (
							<g key={n.id}>
								<rect
									x={n.x}
									y={n.y}
									width={n.width}
									height={n.height}
									rx={NODE_RADIUS}
									fill="#ffffff"
									stroke={isRoot ? ROOT_COLOR : NODE_BORDER}
									strokeWidth={isRoot ? 2 : 1.5}
								/>
								{/* Both labels are placed from the box's own coordinates
								    rather than a shared transform, so each can be nudged
								    independently. */}
								<text
									x={n.x + n.width / 2}
									y={n.y + 22}
									textAnchor="middle"
									fontSize="13"
									fontWeight="600"
									fill="#0f172a"
								>
									{n.task.id}
								</text>
								<text
									x={n.x + n.width / 2}
									y={n.y + 40}
									textAnchor="middle"
									fontSize="11"
									fill="#64748b"
								>
									{n.task.handler}
								</text>
							</g>
						);
					})}
				</g>
			</svg>
		</div>
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
};
