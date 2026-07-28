import assert from "node:assert/strict";
import { test } from "node:test";

import { edgePath, layoutDag, NODE_HEIGHT, NODE_WIDTH } from "./layout.js";

const DIAMOND = [
	{ id: "extract", dependsOn: [] },
	{ id: "transform", dependsOn: ["extract"] },
	{ id: "validate", dependsOn: ["extract"] },
	{ id: "load", dependsOn: ["transform", "validate"] },
];

test("layoutDag positions every task and keeps its box size", () => {
	const { nodes, width, height } = layoutDag(DIAMOND);

	assert.equal(nodes.length, 4);
	for (const node of nodes) {
		assert.equal(node.width, NODE_WIDTH);
		assert.equal(node.height, NODE_HEIGHT);
		assert.ok(Number.isFinite(node.x) && Number.isFinite(node.y), node.id);
	}
	assert.ok(width > 0 && height > 0);
});

test("layoutDag draws upstream above downstream", () => {
	const byId = Object.fromEntries(layoutDag(DIAMOND).nodes.map((n) => [n.id, n]));

	assert.ok(byId.extract.y < byId.transform.y);
	assert.ok(byId.transform.y < byId.load.y);
	// Independent siblings share a rank and are separated horizontally instead.
	assert.equal(byId.transform.y, byId.validate.y);
	assert.notEqual(byId.transform.x, byId.validate.x);
});

test("layoutDag points edges from upstream into the task that waits on it", () => {
	const { edges } = layoutDag(DIAMOND);

	assert.deepEqual(
		edges.map((e) => `${e.from}->${e.to}`).sort(),
		["extract->transform", "extract->validate", "transform->load", "validate->load"],
	);
	for (const edge of edges) {
		assert.ok(edge.points.length >= 2, `${edge.from}->${edge.to}`);
	}
});

test("layoutDag ignores a dependency on a task that is not in the graph", () => {
	// The API rejects these, so reaching dagre with one means the guard is what
	// stands between a bad edge and a node with no width.
	const { nodes, edges } = layoutDag([{ id: "only", dependsOn: ["ghost"] }]);

	assert.deepEqual(
		nodes.map((n) => n.id),
		["only"],
	);
	assert.deepEqual(edges, []);
});

test("layoutDag handles the empty and single-task cases", () => {
	assert.deepEqual(layoutDag([]).nodes, []);
	assert.equal(layoutDag([{ id: "solo" }]).nodes.length, 1);
});

test("edgePath draws a straight line when there is nothing to smooth", () => {
	assert.equal(edgePath([]), "");
	assert.equal(edgePath(undefined), "");
	assert.equal(
		edgePath([
			{ x: 0, y: 0 },
			{ x: 10, y: 20 },
		]),
		"M 0 0 L 10 20",
	);
});

// The API rejects these shapes, so reaching the renderer with one means something
// upstream is already wrong. The graph still has to draw: a thrown exception here
// takes the whole page down, which is a worse way to learn about it than a strange
// looking DAG.
test("layoutDag survives graphs the API would never send", () => {
	const cases = {
		"duplicate ids": [{ id: "a" }, { id: "a" }],
		"self dependency": [{ id: "a", dependsOn: ["a"] }],
		cycle: [
			{ id: "a", dependsOn: ["b"] },
			{ id: "b", dependsOn: ["a"] },
		],
		"null dependsOn": [{ id: "a", dependsOn: null }],
	};

	for (const [label, tasks] of Object.entries(cases)) {
		const { nodes, edges, width, height } = layoutDag(tasks);

		assert.ok(nodes.length >= 1, `${label}: no nodes came back`);
		assert.ok(width > 0 && height > 0, `${label}: empty bounding box`);
		for (const node of nodes) {
			assert.ok(Number.isFinite(node.x) && Number.isFinite(node.y), `${label}: ${node.id} has no position`);
		}
		for (const edge of edges) {
			assert.ok(Array.isArray(edge.points), `${label}: edge without waypoints`);
		}
	}

	// Two tasks with one id are one node: dagre keys on the id, and the second
	// definition wins rather than producing a phantom second box.
	assert.equal(layoutDag([{ id: "a" }, { id: "a" }]).nodes.length, 1);
});

test("edgePath keeps both endpoints", () => {
	// A single waypoint is a degenerate edge, and the path still has to start
	// somewhere rather than be empty or NaN.
	assert.equal(edgePath([{ x: 1, y: 2 }]), "M 1 2");

	const points = [
		{ x: 0, y: 0 },
		{ x: 5, y: 5 },
		{ x: 10, y: 10 },
		{ x: 20, y: 0 },
	];
	const d = edgePath(points);
	assert.ok(d.startsWith("M 0 0"), d);
	assert.ok(d.endsWith("20 0"), `path does not reach its final waypoint: ${d}`);
	assert.ok(!d.includes("NaN"), `path contains NaN: ${d}`);
});

test("edgePath curves through interior waypoints", () => {
	const d = edgePath([
		{ x: 0, y: 0 },
		{ x: 10, y: 10 },
		{ x: 20, y: 0 },
	]);

	assert.ok(d.startsWith("M 0 0"), d);
	assert.ok(d.includes("Q"), d);
});
