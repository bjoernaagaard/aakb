package httpapi

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bjoernaagaard/aakb"
)

func (a *API) serveFile(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, downloadsPath)
	if !validFilename(name) {
		fileNotFound(w)
		return
	}
	root, err := os.OpenRoot(a.client.DownloadDir())
	if err != nil {
		fileNotFound(w)
		return
	}
	defer root.Close() //nolint:errcheck // closing a read-only directory cannot change the response

	// Nonblocking open lets Stat reject FIFOs even if nobody is writing to them.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		fileNotFound(w)
		return
	}
	defer file.Close() //nolint:errcheck // closing a read-only file cannot change the response
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		fileNotFound(w)
		return
	}

	filename := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+filename)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func validFilename(name string) bool {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\\x00") || strings.HasPrefix(name, ".") {
		return false
	}
	ext := filepath.Ext(name)
	return !strings.EqualFold(ext, ".part") && !strings.EqualFold(ext, ".tmp")
}

func fileNotFound(w http.ResponseWriter) {
	writeProblem(w, http.StatusNotFound, string(aakb.CodeNotFound), "The completed file was not found.")
}
