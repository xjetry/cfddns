package cloudflare_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cfddns/internal/cloudflare"
)

func TestZoneIDFor_LongestSuffixMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zones" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization: %q", got)
		}
		if r.URL.Query().Get("per_page") != "50" {
			t.Errorf("per_page: %q", r.URL.Query().Get("per_page"))
		}
		// Zones include a parent and a more specific child; longest match wins.
		writeJSON(w, map[string]any{
			"success": true,
			"result": []map[string]string{
				{"id": "zone-com", "name": "example.com"},
				{"id": "zone-home", "name": "home.example.com"},
				{"id": "zone-other", "name": "other.net"},
			},
			"result_info": map[string]int{
				"page": 1, "per_page": 50, "total_pages": 1, "count": 3, "total_count": 3,
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{
		Token:      "test-token",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	}
	id, err := c.ZoneIDFor(context.Background(), "sub.home.example.com")
	if err != nil {
		t.Fatalf("ZoneIDFor: %v", err)
	}
	if id != "zone-home" {
		t.Fatalf("got zone %q, want zone-home (longest suffix home.example.com)", id)
	}

	// Exact zone name match.
	id, err = c.ZoneIDFor(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("ZoneIDFor exact: %v", err)
	}
	if id != "zone-com" {
		t.Fatalf("got zone %q, want zone-com", id)
	}
}

func TestZoneIDFor_Paginate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			writeJSON(w, map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "zone-a", "name": "example.com"},
				},
				"result_info": map[string]int{
					"page": 1, "per_page": 50, "total_pages": 2, "count": 1, "total_count": 2,
				},
			})
		case "2":
			writeJSON(w, map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "zone-b", "name": "deep.example.com"},
				},
				"result_info": map[string]int{
					"page": 2, "per_page": 50, "total_pages": 2, "count": 1, "total_count": 2,
				},
			})
		default:
			t.Errorf("unexpected page %q", page)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{Token: "t", BaseURL: srv.URL, HTTPClient: srv.Client()}
	id, err := c.ZoneIDFor(context.Background(), "x.deep.example.com")
	if err != nil {
		t.Fatalf("ZoneIDFor: %v", err)
	}
	if id != "zone-b" {
		t.Fatalf("got %q, want zone-b from page 2", id)
	}
}

func TestFindRecords_EmptyVsHits(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/zones/zid/dns_records") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("type") != "A" || r.URL.Query().Get("name") != "home.example.com" {
			t.Errorf("query: %s", r.URL.RawQuery)
		}
		hits++
		if hits == 1 {
			writeJSON(w, map[string]any{
				"success": true,
				"result":  []any{},
			})
			return
		}
		writeJSON(w, map[string]any{
			"success": true,
			"result": []map[string]any{
				{"id": "rec1", "type": "A", "name": "home.example.com", "content": "203.0.113.1", "proxied": false, "ttl": 1},
				{"id": "rec2", "type": "A", "name": "home.example.com", "content": "203.0.113.2", "proxied": true, "ttl": 300},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{Token: "t", BaseURL: srv.URL, HTTPClient: srv.Client()}

	recs, err := c.FindRecords(context.Background(), "zid", "A", "home.example.com")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected no records, got %+v", recs)
	}

	recs, err = c.FindRecords(context.Background(), "zid", "A", "home.example.com")
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if len(recs) != 2 || recs[0].ID != "rec1" || recs[0].Content != "203.0.113.1" || recs[1].ID != "rec2" || !recs[1].Proxied {
		t.Fatalf("got %+v", recs)
	}
}

func TestCreateRecord_ForcesDNSOnlyAndReturnsID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if r.URL.Path != "/zones/zid/dns_records" {
			t.Errorf("path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization: %q", got)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type: %q", ct)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, map[string]any{
			"success": true,
			"result":  map[string]any{"id": "new", "type": "A", "name": "home.example.com", "content": "198.51.100.9", "proxied": false, "ttl": 1},
		})
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{Token: "tok", BaseURL: srv.URL, HTTPClient: srv.Client()}
	rec, err := c.CreateRecord(context.Background(), "zid", "A", "home.example.com", "198.51.100.9")
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if rec.ID != "new" || rec.Content != "198.51.100.9" {
		t.Fatalf("returned record %+v", rec)
	}
	if gotBody["type"] != "A" || gotBody["name"] != "home.example.com" || gotBody["content"] != "198.51.100.9" {
		t.Fatalf("body fields: %#v", gotBody)
	}
	if gotBody["proxied"] != false {
		t.Fatalf("proxied=%v, want false", gotBody["proxied"])
	}
	// JSON numbers decode as float64
	ttl, ok := gotBody["ttl"].(float64)
	if !ok || ttl != 1 {
		t.Fatalf("ttl=%v, want 1", gotBody["ttl"])
	}
}

func TestCreateRecord_MissingIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": true, "result": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{Token: "tok", BaseURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := c.CreateRecord(context.Background(), "zid", "A", "home.example.com", "198.51.100.9"); err == nil {
		t.Fatal("expected error when create returns no id")
	}
}

func TestUpdateRecordContent_PATCHOnlyContent(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, map[string]any{"success": true, "result": map[string]string{"id": "rec1"}})
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{Token: "tok", BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := c.UpdateRecordContent(context.Background(), "zid", "rec1", "2001:db8::1"); err != nil {
		t.Fatalf("UpdateRecordContent: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method %s, want PATCH so dashboard-managed fields survive", gotMethod)
	}
	if gotPath != "/zones/zid/dns_records/rec1" {
		t.Fatalf("path %s", gotPath)
	}
	if len(gotBody) != 1 || gotBody["content"] != "2001:db8::1" {
		t.Fatalf("body must carry only content: %#v", gotBody)
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
		})
	}))
	t.Cleanup(srv.Close)

	c := &cloudflare.Client{Token: "bad", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.ZoneIDFor(context.Background(), "example.com")
	if err == nil || !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("error: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
