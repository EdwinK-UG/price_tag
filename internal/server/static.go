package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type asset struct {
	raw, gz      []byte
	ct, etag, cc string
}

type staticFiles map[string]*asset

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".svg": "image/svg+xml", ".wasm": "application/wasm",
	".webmanifest": "application/manifest+json", ".png": "image/png", ".txt": "text/plain; charset=utf-8",
}

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "svg") || strings.Contains(ct, "wasm") || strings.Contains(ct, "javascript")
}

// loadStatic reads the embedded web files once and pre-compresses them, so
// serving a file is a map lookup and a write: no per-request CPU.
func loadStatic(fsys fs.FS) (staticFiles, error) {
	out := staticFiles{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".md") {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ext := path.Ext(p)
		ct := contentTypes[ext]
		if ct == "" {
			ct = mime.TypeByExtension(ext)
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		sum := sha1.Sum(raw)
		a := &asset{raw: raw, ct: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, cc: "no-cache"}
		// vendored third-party files carry their version in the file name
		if strings.HasPrefix(p, "vendor/") {
			a.cc = "public, max-age=31536000, immutable"
		}
		if compressible(ct) && len(raw) > 400 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(raw)
			zw.Close()
			if buf.Len() < len(raw) {
				a.gz = buf.Bytes()
			}
		}
		out[p] = a
		return nil
	})
	return out, err
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func (a *asset) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", a.ct)
	h.Set("Cache-Control", a.cc)
	h.Set("ETag", a.etag)
	h.Add("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.raw
	if a.gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}
