// Design tokens for the whole UI, in one place.
//
// The components style themselves inline (esbuild is pointed at a single JSX entry
// and emits one bundle.js, so a stylesheet would mean a second output file and a
// <link> to go with it). Inline styles have no cascade, which is fine for layout but
// means a colour used in four components is otherwise written out four times and
// drifts. Everything shared lives here instead, and a component only writes a
// literal colour when it is genuinely local to that component.

export const color = {
	// Three background layers, not one. Depth is what stops a dark interface from
	// reading as a flat void: the page sits furthest back, panels come forward, and
	// the graph canvas is recessed so the nodes drawn on it feel placed on a
	// surface rather than floating.
	bg: "#0a0e15",
	surface: "#111823",
	canvas: "#0c1119",
	raised: "#1b2431",
	raisedHover: "#212c3b",

	border: "#1e2836",
	borderStrong: "#2c3849",

	// Text steps down in three stages. Anything below textFaint stops being legible
	// on these backgrounds, which is why there is no fourth.
	text: "#e6ebf2",
	textMuted: "#93a1b5",
	textFaint: "#66768d",

	accent: "#6366f1",
	accentHover: "#7679f3",
	accentBorder: "#4f46e5",
	accentSoft: "rgba(99, 102, 241, 0.14)",

	danger: "#f87171",
	dangerSoft: "rgba(248, 113, 113, 0.12)",
	dangerBorder: "rgba(248, 113, 113, 0.35)",

	warn: "#fbbf24",
};

export const font = {
	sans: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, system-ui, sans-serif',
	mono: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
};

// Style fragments used in more than one component.
export const shared = {
	// The small uppercase label above each region.
	sectionTitle: {
		margin: "0 0 8px",
		fontSize: 10,
		fontWeight: 600,
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: color.textFaint,
	},

	// Status chips. The caller supplies the three colours from the status palette;
	// this is only the shape.
	pill: {
		display: "inline-block",
		fontSize: 11,
		fontWeight: 600,
		padding: "2px 9px",
		borderRadius: 999,
		borderWidth: 1,
		borderStyle: "solid",
		letterSpacing: "0.01em",
	},

	buttonBase: {
		font: "inherit",
		fontSize: 12.5,
		fontWeight: 500,
		padding: "6px 13px",
		borderRadius: 6,
		cursor: "pointer",
		// Transitions are short enough to feel like feedback rather than animation.
		transition: "background 120ms ease, border-color 120ms ease",
	},

	primaryButton: {
		border: `1px solid ${color.accentBorder}`,
		background: color.accent,
		color: "#ffffff",
	},

	secondaryButton: {
		border: `1px solid ${color.borderStrong}`,
		background: "transparent",
		color: color.textMuted,
	},
};
