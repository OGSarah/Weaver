import { Graph, layout } from "@dagrejs/dagre";

// Everything in this file is pure geometry. dagre never renders anything: you hand
// it nodes with a size and edges between them, it decides where each node goes, and
// you get plain numbers back. Drawing those numbers is entirely up to Dag.jsx.
//
// Keeping the split here is the whole point. The layout problem (which rank does a
// task sit on, how do you order tasks within a rank so edges cross as little as
// possible) is genuinely hard and worth delegating; turning {x, y} into an <rect> is
// not.

// Node box size in layout units. dagre needs a size up front because it reserves
// space for each node before it knows what you will draw inside.
export const NODE_WIDTH = 172;
export const NODE_HEIGHT = 56;

// Padding around the whole graph, so the outermost nodes are not flush against the
// edge of the drawing.
const MARGIN = 24;

/**
 * layoutDag turns a workflow's task list into positioned nodes and routed edges.
 *
 * Returns { nodes, edges, width, height }:
 *  - nodes: the task plus x/y of its top-left corner (dagre reports centers; the
 *    conversion happens here so the renderer can use x/y directly on an <rect>).
 *  - edges: { from, to, points } where points are the waypoints dagre routed,
 *    already clipped to the node borders rather than their centers.
 *  - width/height: the graph's bounding box, which becomes the SVG viewBox.
 */
export function layoutDag(tasks) {
	const g = new Graph();

	// rankdir TB draws roots at the top and dependencies flowing down, which matches
	// how the README draws DAGs and how people read them. ranksep is the gap between
	// dependency levels, nodesep the gap between siblings on the same level.
	g.setGraph({ rankdir: "TB", ranksep: 64, nodesep: 36, marginx: MARGIN, marginy: MARGIN });

	// dagre supports labels on edges; we have none, so every edge gets an empty one.
	// Without this default it would ask for a label object per edge.
	g.setDefaultEdgeLabel(() => ({}));

	// Nodes first. The id is the handle dagre uses for edges, and the label object is
	// handed straight back to us after layout with x/y filled in.
	for (const task of tasks) {
		g.setNode(task.id, { task, width: NODE_WIDTH, height: NODE_HEIGHT });
	}

	// Then edges. A task's dependsOn lists what must finish first, so the arrow points
	// from the upstream task into this one: upstream -> task, the direction work flows.
	for (const task of tasks) {
		for (const upstream of task.dependsOn ?? []) {
			// The API validates that every dependsOn names a real task, so this should
			// never skip anything. The guard exists because setEdge would otherwise
			// silently invent a node with no width or height and break the layout.
			if (g.hasNode(upstream)) {
				g.setEdge(upstream, task.id);
			}
		}
	}

	// One call does the real work: assign ranks, order within ranks, assign
	// coordinates, and route the edges.
	layout(g);

	const nodes = g.nodes().map((id) => {
		const n = g.node(id);
		// dagre positions nodes by their center. SVG rects are positioned by their
		// top-left corner, so shift by half the box once, here.
		return {
			id,
			task: n.task,
			x: n.x - n.width / 2,
			y: n.y - n.height / 2,
			width: n.width,
			height: n.height,
		};
	});

	const edges = g.edges().map((e) => ({
		from: e.v,
		to: e.w,
		points: g.edge(e).points,
	}));

	// dagre records the bounding box it used, including the margins set above.
	const { width, height } = g.graph();

	return { nodes, edges, width, height };
}

/**
 * edgePath converts dagre's waypoints into an SVG path string.
 *
 * Joining the points with straight lines works but looks harsh at every bend. The
 * trick used here is the standard one: run a quadratic curve through each interior
 * point, using that point as the control point and the midpoint of each segment as
 * the on-curve anchor. The line still passes near every waypoint, so the routing
 * dagre computed (which is what keeps edges from cutting through nodes) is
 * preserved, but the corners come out rounded.
 */
export function edgePath(points) {
	if (!points || points.length === 0) {
		return "";
	}
	if (points.length < 3) {
		// Nothing to smooth: a single straight segment.
		return points.map((p, i) => `${i === 0 ? "M" : "L"} ${p.x} ${p.y}`).join(" ");
	}

	let d = `M ${points[0].x} ${points[0].y}`;
	for (let i = 1; i < points.length - 1; i++) {
		const p = points[i];
		const next = points[i + 1];
		const midX = (p.x + next.x) / 2;
		const midY = (p.y + next.y) / 2;
		d += ` Q ${p.x} ${p.y} ${midX} ${midY}`;
	}
	// Finish on the last waypoint, which dagre already clipped to the target's border.
	const last = points[points.length - 1];
	d += ` L ${last.x} ${last.y}`;
	return d;
}
