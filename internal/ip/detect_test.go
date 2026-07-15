package ip_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cfddns/internal/ip"
)

func TestParseTraceIP(t *testing.T) {
	body := "fl=xxx\nh=1.1.1.1\nip=203.0.113.10\nts=1.0\n"
	got, err := ip.ParseTraceIP(body)
	if err != nil || got != "203.0.113.10" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestParseTraceIP_Missing(t *testing.T) {
	_, err := ip.ParseTraceIP("fl=1\n")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseTraceIP_IPv6(t *testing.T) {
	body := "ip=2001:db8::1\n"
	got, err := ip.ParseTraceIP(body)
	if err != nil || !strings.Contains(got, ":") {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDetector_Detect_ClientFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/trace" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("fl=xxx\nip=198.51.100.42\nts=1.0\n"))
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	d := &ip.Detector{
		ClientFor: func(f ip.Family) *http.Client {
			if f != ip.IPv4 {
				t.Errorf("expected IPv4, got %v", f)
			}
			return &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					req2 := req.Clone(req.Context())
					req2.URL.Scheme = target.Scheme
					req2.URL.Host = target.Host
					req2.Host = target.Host
					return http.DefaultTransport.RoundTrip(req2)
				}),
			}
		},
	}

	got, err := d.Detect(context.Background(), ip.IPv4)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != "198.51.100.42" {
		t.Fatalf("got %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFamily_String(t *testing.T) {
	if ip.IPv4.String() != "ipv4" {
		t.Fatalf("IPv4: %q", ip.IPv4.String())
	}
	if ip.IPv6.String() != "ipv6" {
		t.Fatalf("IPv6: %q", ip.IPv6.String())
	}
}
