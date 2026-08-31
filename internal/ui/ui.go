package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// The browser-native UI is committed with the Go source and embedded in every
// build. It has no generated bundle and no external runtime assets.
//
//go:embed static/*
var content embed.FS

func RegisterRoutes(mux *http.ServeMux) error {
	handler, err := newEmbeddedHandler(content)
	if err != nil {
		return err
	}
	mux.Handle("/", handler)
	return nil
}

var reservedRoots = []string{"/api", "/v1", "/ws", "/mcp", "/metrics"}

type embeddedAsset struct {
	body []byte
	etag string
}

type embeddedHandler struct {
	assets map[string]embeddedAsset
	index  embeddedAsset
}

func newEmbeddedHandler(source fs.FS) (*embeddedHandler, error) {
	entries, err := fs.ReadDir(source, "static")
	if err != nil {
		return nil, fmt.Errorf("ui: read embedded assets: %w", err)
	}

	h := &embeddedHandler{assets: make(map[string]embeddedAsset, len(entries))}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		body, readErr := fs.ReadFile(source, "static/"+name)
		if readErr != nil {
			return nil, fmt.Errorf("ui: read %s: %w", name, readErr)
		}
		sum := sha256.Sum256(body)
		asset := embeddedAsset{
			body: body,
			etag: "\"" + hex.EncodeToString(sum[:8]) + "\"",
		}
		h.assets[name] = asset
		if name == "index.html" {
			h.index = asset
		}
	}
	if len(h.index.body) == 0 {
		return nil, fmt.Errorf("ui: embedded index.html is missing")
	}
	return h, nil
}

func (h *embeddedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cleanPath := path.Clean("/" + strings.Trim(r.URL.Path, "/"))
	if strings.HasPrefix(cleanPath, "/static/") {
		h.serveAsset(w, r, strings.TrimPrefix(cleanPath, "/static/"))
		return
	}
	if cleanPath == "/" || cleanPath == "/index.html" {
		h.serveIndex(w, r)
		return
	}
	if strings.Contains(strings.TrimPrefix(cleanPath, "/"), ".") || isReserved(cleanPath) {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

func (h *embeddedHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	serveEmbedded(w, r, "index.html", h.index)
}

func (h *embeddedHandler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	asset, ok := h.assets[name]
	if !ok || name == "index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	serveEmbedded(w, r, name, asset)
}

func serveEmbedded(w http.ResponseWriter, r *http.Request, name string, asset embeddedAsset) {
	w.Header().Set("ETag", asset.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(asset.body))
}

func isReserved(requestPath string) bool {
	for _, root := range reservedRoots {
		if requestPath == root || strings.HasPrefix(requestPath, root+"/") {
			return true
		}
	}
	return false
}
