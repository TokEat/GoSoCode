// Package webui embeds the built frontend (frontend/dist copied into static/)
// so both the server and the CLI client binaries ship the same web UI without
// external files. Rebuild flow: `npm run build` in frontend/, then copy dist/*
// into backend/internal/webui/static/.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:static
var static embed.FS

// FS returns the embedded UI, or ok=false when only the placeholder is present.
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

// Handler serves the embedded UI with SPA fallback to index.html for unknown
// paths (client-side routing). Returns nil when no UI is embedded.
func Handler() http.Handler {
	sub, ok := FS()
	if !ok {
		return nil
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err == nil {
			files.ServeHTTP(w, r)
			return
		}
		f, err := sub.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
	})
}
