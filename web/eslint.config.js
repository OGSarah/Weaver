import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";

// Flat config (ESLint 9). Two blocks: the browser code under src/, and the Node
// test files that run under `node --test`, which need Node globals rather than
// browser ones.
export default [
	{ ignores: ["dist/**", "node_modules/**"] },
	{
		files: ["src/**/*.js", "src/**/*.jsx"],
		languageOptions: {
			ecmaVersion: 2023,
			sourceType: "module",
			globals: globals.browser,
			parserOptions: { ecmaFeatures: { jsx: true } },
		},
		plugins: { "react-hooks": reactHooks },
		rules: {
			...js.configs.recommended.rules,
			"react-hooks/rules-of-hooks": "error",
			"react-hooks/exhaustive-deps": "error",
			// JSX compiles to React.createElement (esbuild's classic transform), so the
			// React import is used even where no rule can see it referenced.
			"no-unused-vars": ["error", { varsIgnorePattern: "^React$" }],
		},
	},
	{
		files: ["**/*.test.js"],
		languageOptions: {
			ecmaVersion: 2023,
			sourceType: "module",
			globals: globals.node,
		},
		rules: js.configs.recommended.rules,
	},
];
