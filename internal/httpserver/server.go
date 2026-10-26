package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"cfddns/internal/updater"
)

const (
	// RunTimeout bounds one manual update cycle: two trace requests (10s
	// each) plus a full zone/record resolve of three 30s-bounded API calls
	// still fit, so a stalled upstream ends the request instead of holding
	// the connection and the updater lock open indefinitely.
	RunTimeout = 2 * time.Minute

	// Header/body reads are tiny for this endpoint; short bounds keep a slow
	// client from pinning a connection.
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

// Server exposes a manual DDNS update endpoint authenticated with a shared secret.
type Server struct {
	Passwd string
	Run    func(ctx context.Context) ([]updater.Result, bool)
}

// Handler returns the HTTP handler for this server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/update", s.handleUpdate)
	return mux
}

// HTTPServer builds the listener with every timeout set. Callers must use it
// instead of constructing http.Server themselves so the bounds live in one
// place next to the handler deadline they have to agree with.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		// WriteTimeout runs from the end of the request headers, so it must
		// outlast the handler's own deadline or a slow cycle's response is
		// cut off after the work was done.
		WriteTimeout: RunTimeout + 15*time.Second,
		IdleTimeout:  idleTimeout,
	}
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r.Header.Get("Authorization"), s.Passwd) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), RunTimeout)
	defer cancel()
	results, ok := s.Run(ctx)
	// Always 200: partial family failures are reported in the JSON body (ok:false).
	writeJSON(w, http.StatusOK, response{OK: ok, Results: results})
}

type response struct {
	OK      bool             `json:"ok"`
	Results []updater.Result `json:"results"`
}

func bearerOK(header, passwd string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	// ConstantTimeCompare returns 0 when lengths differ (no panic).
	return subtle.ConstantTimeCompare([]byte(got), []byte(passwd)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
