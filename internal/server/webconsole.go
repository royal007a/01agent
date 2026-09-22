package server

import (
	"embed"
	"net/http"
	"path"
)

//go:embed web/index.html web/app.js web/styles.css
var webConsoleFiles embed.FS

func (h *Handler) webConsole(writer http.ResponseWriter, request *http.Request) {
	serveWebConsoleFile(writer, request, "web/index.html", "text/html; charset=utf-8", "no-store")
}

func (h *Handler) webConsoleAsset(writer http.ResponseWriter, request *http.Request) {
	name := path.Base(request.URL.Path)
	contentType := "application/octet-stream"
	switch name {
	case "app.js":
		contentType = "text/javascript; charset=utf-8"
	case "styles.css":
		contentType = "text/css; charset=utf-8"
	default:
		http.NotFound(writer, request)
		return
	}
	serveWebConsoleFile(writer, request, "web/"+name, contentType, "public, max-age=300")
}

func serveWebConsoleFile(writer http.ResponseWriter, request *http.Request, name, contentType, cacheControl string) {
	content, err := webConsoleFiles.ReadFile(name)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", cacheControl)
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(content)
}
