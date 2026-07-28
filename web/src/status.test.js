import assert from "node:assert/strict";
import { test } from "node:test";

import {
	breakdown,
	countByStatus,
	isRunFinished,
	statusColors,
	STATUS_COLORS,
	STATUS_ORDER,
} from "./status.js";

test("statusColors falls back rather than returning undefined", () => {
	assert.equal(statusColors("running"), STATUS_COLORS.running);

	// A status the schema grows before the palette does must still render.
	const unknown = statusColors("quarantined");
	assert.ok(unknown.fill && unknown.stroke && unknown.text);
});

test("every ordered status has a colour, and every colour is ordered", () => {
	assert.deepEqual([...STATUS_ORDER].sort(), Object.keys(STATUS_COLORS).sort());
});

test("breakdown orders by lifecycle and drops empty statuses", () => {
	assert.deepEqual(breakdown({ succeeded: 2, pending: 1, running: 0, dead: 3 }), [
		["pending", 1],
		["succeeded", 2],
		["dead", 3],
	]);
});

test("breakdown keeps unknown statuses so the counts still add up", () => {
	const rows = breakdown({ running: 1, quarantined: 2 });

	assert.deepEqual(rows, [
		["running", 1],
		["quarantined", 2],
	]);
	assert.equal(
		rows.reduce((sum, [, n]) => sum + n, 0),
		3,
	);
});

test("breakdown tolerates a missing counts map", () => {
	assert.deepEqual(breakdown(null), []);
	assert.deepEqual(breakdown(undefined), []);
});

test("countByStatus tallies a run's task list", () => {
	const tasks = [
		{ id: "a", status: "succeeded" },
		{ id: "b", status: "succeeded" },
		{ id: "c", status: "failed" },
	];

	assert.deepEqual(countByStatus(tasks), { succeeded: 2, failed: 1 });
	assert.deepEqual(countByStatus([]), {});
});

test("isRunFinished stops polling only on terminal run statuses", () => {
	for (const status of ["succeeded", "failed", "cancelled"]) {
		assert.equal(isRunFinished(status), true, status);
	}
	for (const status of ["pending", "running", undefined]) {
		assert.equal(isRunFinished(status), false, String(status));
	}
});
