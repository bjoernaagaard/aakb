// Package webui serves the embedded aakb browser interface.
package webui

import (
	"bytes"
	"embed"
	"net/http"
	"time"

	"github.com/bjoernaagaard/aakb/assets"
)

//go:embed static
var static embed.FS

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

type file struct {
	contentType string
	body        []byte
}

// Handler serves the UI shell at GET / and its allowlisted assets at
// GET /ui/{file}, with a strict Content-Security-Policy. Any other file name
// returns 404. The handler is safe for concurrent use.
func Handler() http.Handler {
	files := map[string]file{
		"index.html": {"text/html; charset=utf-8", mustRead("static/index.html")},
		"app.js":     {"text/javascript; charset=utf-8", mustRead("static/app.js")},
		"app.css":    {"text/css; charset=utf-8", mustRead("static/app.css")},
		"icon.svg":   {"image/svg+xml", assets.Icon},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if name == "" || name == "index.html" {
			name = "index.html"
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
		}
		f, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", f.contentType)
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.body))
	})
}

// mustRead reads an embedded file. The files are fixed at build time, so a
// failure is a programmer error.
func mustRead(name string) []byte {
	b, err := static.ReadFile(name)
	if err != nil {
		panic("webui: missing embedded " + name)
	}
	return b
}
