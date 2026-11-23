The production frontend is embedded from `internal/webui/assets` so the Wails
desktop app and the browser lab share exactly the same source. This directory
exists only because the Wails CLI expects a conventional project layout; no
Node.js/npm build step is required.
