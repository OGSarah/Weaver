import React from "react";
import { createRoot } from "react-dom/client";

// Placeholder component. Just enough to prove the whole chain works: bundler, JSX transform, and React mounting into the page.
function App() {
    return (
        <div>
            <h1>Weaver</h1>
            <p>Frontend is wired up.</p>
        </div>
    );
}

// Find the mount point from index.html and hand it to React.
const container = document.getElementById("root");
const root = createRoot(container);
root.render(<App />);