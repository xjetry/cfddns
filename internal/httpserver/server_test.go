package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"cfddns/internal/httpserver"
	"cfddns/internal/updater"
)

func TestUpdate_Unauthorized(t *testing.T) {
	s := &httpserver.Server{
		Passwd: "secret",
		Run: func(ctx context.Context) ([]updater.Result, bool) {
			t.Fatal("Run must not be called without auth")
			return nil, false
		},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// Missing Authorization
	resp, err := http.Get(ts.URL + "/update")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth: status = %d, want 401", resp.StatusCode)
	}

	// Wrong token
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", resp.StatusCode)
	}
}

func TestUpdate_OK(t *testing.T) {
	s := &httpserver.Server{
		Passwd: "secret",
		Run: func(ctx context.Context) ([]updater.Result, bool) {
			return []updater.Result{
				{Family: "v4", Domain: "a.example.com", IP: "203.0.113.1", Action: "updated"},
			}, true
		},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, ts.URL+"/update", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}

			var body struct {
				OK      bool             `json:"ok"`
				Results []updater.Result `json:"results"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !body.OK {
				t.Fatal("ok = false, want true")
			}
			if len(body.Results) != 1 || body.Results[0].Action != "updated" {
				t.Fatalf("results = %+v", body.Results)
			}
		})
	}
}

func TestUpdate_PartialFailure_Still200(t *testing.T) {
	s := &httpserver.Server{
		Passwd: "secret",
		Run: func(ctx context.Context) ([]updater.Result, bool) {
			return []updater.Result{
				{Family: "v4", Domain: "a.example.com", IP: "203.0.113.1", Action: "updated"},
				{Family: "v6", Domain: "b.example.com", Action: "error", Error: "no route"},
			}, false
		},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on partial failure", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		OK      bool             `json:"ok"`
		Results []updater.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.OK {
		t.Fatal("ok = true, want false for partial failure")
	}
	if len(body.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(body.Results))
	}
	if body.Results[1].Action != "error" || body.Results[1].Error != "no route" {
		t.Fatalf("results[1] = %+v", body.Results[1])
	}
}
