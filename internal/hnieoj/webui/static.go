package webui

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*
var staticFiles embed.FS

func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return spaHandler{fs: sub}
}

type spaHandler struct {
	fs fs.FS
}

var spaRoutes = map[string]struct{}{
	"/":                 {},
	"/setup-password":   {},
	"/login":            {},
	"/configure":        {},
	"/configure/formal": {},
	"/configure/temp":   {},
	"/dashboard":        {},
	"/operations":       {},
	"/logs":             {},
	"/cache":            {},
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	file, err := h.fs.Open(name)
	if err != nil {
		if isSPARoute(r.URL.Path) {
			h.serveIndex(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		if isSPARoute(r.URL.Path) {
			h.serveIndex(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, stat.ModTime(), bytes.NewReader(body))
}

func isSPARoute(route string) bool {
	route = path.Clean(route)
	if route == "." {
		route = "/"
	}
	_, ok := spaRoutes[route]
	return ok
}

func (h spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	file, err := h.fs.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, file)
}
