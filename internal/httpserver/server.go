package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"cfddns/internal/updater"
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

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r.Header.Get("Authorization"), s.Passwd) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	results, ok := s.Run(r.Context())
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
