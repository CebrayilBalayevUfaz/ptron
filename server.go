package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

// Server wires configuration, the virsh wrapper and HTTP handlers together.
type Server struct {
	cfg      *Config
	virsh    *Virsh
	sessions *sessionStore
	tokens   *consoleTokens
	static   fs.FS
}

func NewServer(cfg *Config, v *Virsh) *Server {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return &Server{
		cfg:      cfg,
		virsh:    v,
		sessions: newSessionStore(cfg.SessionTTL),
		tokens:   newConsoleTokens(),
		static:   sub,
	}
}

func (s *Server) Close() {}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Pages
	mux.HandleFunc("GET /login", s.servePage("login.html"))
	mux.Handle("GET /{$}", s.requirePageAuth(s.servePage("index.html")))
	mux.Handle("GET /console", s.requirePageAuth(s.servePage("console.html")))
	mux.Handle("GET /static/", http.FileServerFS(s.static))

	// noVNC (served from disk)
	if s.cfg.NoVNCDir != "" {
		mux.Handle("GET /novnc/", s.requirePageAuth(http.StripPrefix("/novnc/", http.FileServer(http.Dir(s.cfg.NoVNCDir)))))
	} else {
		mux.HandleFunc("GET /novnc/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "noVNC is not installed on the server (see README)", http.StatusNotFound)
		})
	}

	// Auth
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)

	// API
	api := http.NewServeMux()
	api.HandleFunc("GET /api/host", s.handleHost)
	api.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	api.HandleFunc("GET /api/vms", s.handleListVMs)
	api.HandleFunc("POST /api/vms", s.handleCreateVM)
	api.HandleFunc("GET /api/vms/{name}", s.handleGetVM)
	api.HandleFunc("DELETE /api/vms/{name}", s.handleUndefine)
	api.HandleFunc("GET /api/vms/{name}/stats", s.handleStats)
	api.HandleFunc("GET /api/vms/{name}/xml", s.handleGetXML)
	api.HandleFunc("PUT /api/vms/{name}/xml", s.handlePutXML)
	api.HandleFunc("POST /api/vms/{name}/action", s.handleAction)
	api.HandleFunc("GET /api/vms/{name}/snapshots", s.handleListSnapshots)
	api.HandleFunc("POST /api/vms/{name}/snapshots", s.handleCreateSnapshot)
	api.HandleFunc("POST /api/vms/{name}/snapshots/{snap}/revert", s.handleRevertSnapshot)
	api.HandleFunc("DELETE /api/vms/{name}/snapshots/{snap}", s.handleDeleteSnapshot)
	api.HandleFunc("POST /api/vms/{name}/console", s.handleConsoleToken)
	api.HandleFunc("GET /api/networks", s.handleNetworks)
	api.HandleFunc("GET /api/pools", s.handlePools)
	api.HandleFunc("GET /api/pools/{pool}/volumes", s.handleVolumes)
	api.HandleFunc("GET /api/isos", s.handleISOs)
	mux.Handle("/api/", s.requireAPIAuth(api))

	// websockify-compatible proxy
	mux.HandleFunc("GET /ws/vnc/{token}", s.handleVNCWebSocket)

	return logRequests(secureHeaders(mux))
}

func (s *Server) servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, s.static, name)
	}
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/novnc/") {
			return
		}
		if r.Method == http.MethodGet && rw.status < 400 && (r.URL.Path == "/api/vms" || r.URL.Path == "/api/host" || strings.HasSuffix(r.URL.Path, "/stats")) {
			return // too chatty: polling endpoints
		}
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rw.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack delegates to the underlying writer so the WebSocket upgrader works
// through this wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeVirshError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	msg := err.Error()
	var ve *VirshError
	if errors.As(err, &ve) {
		status = http.StatusBadGateway
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, "failed to get domain"), strings.Contains(low, "not found"):
			status = http.StatusNotFound
		case strings.Contains(low, "already active"), strings.Contains(low, "not running"), strings.Contains(low, "is not"), strings.Contains(low, "requested operation is not valid"):
			status = http.StatusConflict
		}
	} else if strings.HasPrefix(msg, "invalid") {
		status = http.StatusBadRequest
	}
	writeError(w, status, msg)
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
