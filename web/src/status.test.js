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
	// Anything it does not recognise keeps the UI polling. Stopping on an
	// unrecognised status would freeze a run mid-flight with no way to notice.
	for (const status of ["pending", "running", "Succeeded", "SUCCEEDED", "", undefined, null, 0]) {
		assert.equal(isRunFinished(status), false, String(status));
	}
});

// The counts come off the wire, so the shapes here are what a stale tab, a
// half-written run, or a future schema can produce. None of them should render as
// a row claiming zero tasks are in some state.
test("breakdown drops statuses nothing is in, including negative counts", () => {
	assert.deepEqual(breakdown({ pending: 0, running: 0 }), []);
	assert.deepEqual(breakdown({}), []);
	// A negative count is nonsense; showing it would be worse than dropping it.
	assert.deepEqual(breakdown({ running: -1, succeeded: 2 }), [["succeeded", 2]]);
	assert.deepEqual(breakdown({ mystery: -3 }), []);
});

// Unknown statuses are appended in a stable order rather than whatever order the
// object happened to arrive in, so the legend does not reshuffle between polls.
test("breakdown orders unknown statuses deterministically", () => {
	const first = breakdown({ zebra: 1, alpha: 2, running: 3 });
	const second = breakdown({ alpha: 2, running: 3, zebra: 1 });

	assert.deepEqual(first, [
		["running", 3],
		["alpha", 2],
		["zebra", 1],
	]);
	assert.deepEqual(first, second);
});

test("countByStatus keeps a task whose status is missing rather than losing it", () => {
	// The total has to stay honest even when a task carries something unexpected:
	// a task silently dropped from the counts is a run that looks smaller than it
	// is. It surfaces as an unknown status, which breakdown then shows.
	const counts = countByStatus([{ id: "a" }, { id: "b", status: "running" }]);

	assert.equal(
		Object.values(counts).reduce((sum, n) => sum + n, 0),
		2,
	);
	assert.equal(counts.running, 1);
});
