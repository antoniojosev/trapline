// Package webui serves the panel, compiled into the binary.
//
// The built assets are committed to the repository and embedded with
// embed.FS. That is a deliberate trade: a checked-in build output is noise in
// a diff, but the alternative is that `go build` needs Node installed, and a
// Go project that cannot be built with the Go toolchain alone is a Go project
// people stop contributing to. The .gitattributes file marks the output as
// generated so reviews collapse it.
//
// Rebuild it with `make web`.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// Handler serves the panel.
//
// Two behaviours, and the second is the one that matters: hashed-free asset
// paths get a long cache, and every unknown path returns index.html so the
// client-side router can handle it. Without that fallback, a reload on any
// route but "/" is a 404 — the classic single-page-app deployment bug.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, err //nolint:wrapcheck // fs.Sub's error is already specific.
	}

	files := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		if path != "" {
			if _, err := fs.Stat(root, path); err == nil {
				// Assets are emitted with stable names rather than content
				// hashes, so they cannot be cached immutably — a rebuild reuses
				// the same filename. must-revalidate keeps a stale panel from
				// outliving a server upgrade, which is the failure that would
				// actually cost someone an afternoon.
				if strings.HasPrefix(path, "assets/") {
					w.Header().Set("Cache-Control", "no-cache, must-revalidate")
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		index, err := fs.ReadFile(root, "index.html")
		if err != nil {
			http.Error(w, "panel not built", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		_, _ = w.Write(index)
	}), nil
}
