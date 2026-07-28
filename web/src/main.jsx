import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";

// Placeholder component. Just enough to prove the whole chain works: bundler, JSX transform, and React mounting into the page.
//
// The fetch is the second half of that proof. Because the Go API serves this bundle,
// "/api/workflows" is same-origin: no host, no port, no CORS. JSON coming back means
// static serving, the API, and the database are all wired together.
function App() {
    const [status, setStatus] = useState("loading");
    const [workflows, setWorkflows] = useState([]);

    useEffect(() => {
        // Abort on unmount so a slow response cannot set state on a gone component.
        const controller = new AbortController();

        fetch("/api/workflows", { signal: controller.signal })
            .then((res) => {
                // fetch only rejects on network failure, so a 4xx or 5xx has to be
                // turned into an error by hand.
                if (!res.ok) {
                    throw new Error(`${res.status} ${res.statusText}`);
                }
                return res.json();
            })
            .then((data) => {
                console.log("GET /api/workflows ->", data);
                setWorkflows(data);
                setStatus("ok");
            })
            .catch((err) => {
                if (err.name === "AbortError") {
                    return;
                }
                console.error("GET /api/workflows failed:", err);
                setStatus(`failed (${err.message})`);
            });

        return () => controller.abort();
    }, []);

    return (
        <div>
            <h1>Weaver</h1>
            <p>Frontend is wired up.</p>
            <p>
                API: {status}
                {status === "ok" && `, ${workflows.length} workflow(s)`}
            </p>
        </div>
    );
}

// Find the mount point from index.html and hand it to React.
const container = document.getElementById("root");
const root = createRoot(container);
root.render(<App />);
