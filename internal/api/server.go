package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"weaver/internal/store"
)

// Server is the HTTP surface over the store. It holds no state of its own beyond
// the store handle; every request reads or writes the database directly, which is
// what keeps the API a thin layer over the single source of truth.
type Server struct {
	store  *store.Store
	webDir string
}

// NewServer builds a Server backed by the given store, serving the built frontend
// out of webDir.
func NewServer(s *store.Store, webDir string) *Server {
	return &Server{store: s, webDir: webDir}
}

// Handler wires the routes onto a ServeMux. The method+path patterns are the
// standard-library router (Go 1.22+): no framework needed, and path wildcards like
// {id} are read back with r.PathValue.
//
// Every API route lives under /api so everything else can fall through to the static
// file server. That one prefix is what lets the UI and the API share an origin: no
// CORS, no dev proxy, and the browser can fetch a bare "/api/workflows".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/workflows", s.handleRegisterWorkflow)
	mux.HandleFunc("GET /api/workflows", s.handleListWorkflows)
	mux.HandleFunc("GET /api/workflows/{id}", s.handleGetWorkflow)
	mux.HandleFunc("POST /api/workflows/{id}/runs", s.handleTriggerRun)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/tasks/{tid}", s.handleGetTask)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)

	// Health stays at the root. It is an infrastructure probe for Compose and load
	// balancers rather than part of the UI's API surface, and an exact pattern still
	// wins over the catch-all below.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Anything under /api that matched no route above. Without this a mistyped path
	// would fall through to the file server and answer a fetch with an HTML 404 body
	// where the caller is expecting JSON.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API endpoint")
	})

	// Everything else is the UI. ServeMux matches the most specific pattern, so "/"
	// only ever sees requests no route above claimed.
	mux.Handle("/", http.FileServer(http.Dir(s.webDir)))

	return logRequests(mux)
}

// writeJSON encodes v as the response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("api: encode response: %v", err)
		}
	}
}

// errorResponse is the uniform shape of every error body, so clients can rely on
// one field regardless of which endpoint failed.
type errorResponse struct {
	Error string `json:"error"`
}

// writeError sends a JSON error with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeStoreError maps a store error to the right HTTP status: not-found and
// not-cancellable become client errors, anything else a 500 that is logged but not
// leaked in detail to the caller.
func writeStoreError(w http.ResponseWriter, context string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNotCancellable):
		writeError(w, http.StatusConflict, "run is not in a cancellable state")
	default:
		log.Printf("api: %s: %v", context, err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// decodeJSON reads and strictly decodes the request body into v, rejecting unknown
// fields so a typo in a workflow definition is a clear error rather than silently
// ignored.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// handleHealth is a trivial liveness probe for Compose and load balancers.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// logRequests logs one line per request with method, path, and status, using a
// small wrapper to capture the status code the handler wrote.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("api: %s %s -> %d", r.Method, r.URL.Path, rec.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
