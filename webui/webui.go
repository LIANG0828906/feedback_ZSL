package webui

import "embed"

// Files contains the complete offline demo UI. No external network resources
// are referenced by the application.
//
//go:embed index.html app.js styles.css
var Files embed.FS
