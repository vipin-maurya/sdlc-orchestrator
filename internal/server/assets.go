package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// templateFS and staticFS are separate embeds so a template can never be
// reached through /static/: the static handler serves only what is in staticFS,
// and page templates are not in it.
//
//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// staticPrefix is both the route prefix and the key prefix of servedAssets.
const staticPrefix = "/static/"

// servedAsset is one embedded file, already read and already hashed. Reading
// them once at startup rather than per request is not an optimisation: it is
// what lets the static handler answer purely from a map, so a hostile path
// never reaches a filesystem call at all.
type servedAsset struct {
	body        []byte
	contentType string
}

// buildAssets reads every file under static/ and returns, first, the map from
// plain name ("app.css") to the URL that carries its content hash
// ("/static/app.<hash>.css"), and second, the map from that URL back to the
// bytes.
//
// The hash is in the URL rather than in a query string because the response is
// served immutable for a year: a query string is not part of the cache key for
// every intermediary, and an operator who upgrades the binary and then reads a
// stale stylesheet has no way to tell that is what happened.
func buildAssets() (map[string]string, map[string]servedAsset, error) {
	names := map[string]string{}
	served := map[string]servedAsset{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		name := strings.TrimPrefix(p, "static/")
		ext := path.Ext(name)
		url := staticPrefix + strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:4]) + ext
		names[name] = url
		served[url] = servedAsset{body: body, contentType: contentTypeFor(ext)}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading embedded static assets: %w", err)
	}
	return names, served, nil
}

// contentTypeFor is a closed table rather than mime.TypeByExtension: the OS
// mime database differs between machines, and a stylesheet served as
// text/plain on one developer's box is a bug that only reproduces there.
func contentTypeFor(ext string) string {
	switch ext {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

// serveStatic answers /static/{path...} from the prebuilt map. An unknown path
// is a 404 and nothing else: there is no fallback to the filesystem, so path
// traversal has nothing to traverse.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	a, ok := s.served[r.URL.Path]
	if !ok {
		s.fail(w, r, http.StatusNotFound, "no such asset")
		return
	}
	w.Header().Set("Content-Type", a.contentType)
	// Safe only because the hash is in the URL: a changed file gets a new URL,
	// so nothing cached under the old one can ever be wrong.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(a.body)
}
