package ip_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// traceServer serves a trace body and returns a ClientFor that redirects the
// hard-coded Cloudflare URL to it.
func traceServer(t *testing.T, body string) func(ip.Family) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/trace" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return func(ip.Family) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				req2 := req.Clone(req.Context())
				req2.URL.Scheme = target.Scheme
				req2.URL.Host = target.Host
				req2.Host = target.Host
				return http.DefaultTransport.RoundTrip(req2)
			}),
		}
	}
}

func TestDetector_Detect_Trace(t *testing.T) {
	clientFor := traceServer(t, "fl=xxx\nip=198.51.100.42\nts=1.0\n")
	d := &ip.Detector{
		ClientFor: func(f ip.Family) *http.Client {
			if f != ip.IPv4 {
				t.Errorf("expected IPv4, got %v", f)
			}
			return clientFor(f)
		},
	}
	got, err := d.Detect(context.Background(), ip.IPv4)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Addr != "198.51.100.42" || got.Source != "trace" {
		t.Fatalf("got %+v", got)
	}
}

func TestDetector_Detect_TraceErrors(t *testing.T) {
	d := &ip.Detector{ClientFor: traceServer(t, "fl=xxx\nts=1.0\n")}
	if _, err := d.Detect(context.Background(), ip.IPv4); err == nil {
		t.Fatal("expected error for body without ip=")
	}
	d = &ip.Detector{ClientFor: func(ip.Family) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no route to host")
		})}
	}}
	_, err := d.Detect(context.Background(), ip.IPv6)
	if err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("err = %v", err)
	}
}

// 1.1.1.1 is blocked on some networks; www.cloudflare.com must be asked next.
func TestDetector_Detect_FallsBackToWWW(t *testing.T) {
	cases := []struct {
		f    ip.Family
		addr string
		want []string
	}{
		{ip.IPv4, "203.0.113.20", []string{"https://1.1.1.1/cdn-cgi/trace", "https://www.cloudflare.com/cdn-cgi/trace"}},
		{ip.IPv6, "2001:db8::20", []string{"https://[2606:4700:4700::1111]/cdn-cgi/trace", "https://www.cloudflare.com/cdn-cgi/trace"}},
	}
	for _, c := range cases {
		t.Run(c.f.String(), func(t *testing.T) {
			var asked []string
			d := &ip.Detector{ClientFor: func(f ip.Family) *http.Client {
				if f != c.f {
					t.Errorf("client for %v, want %v", f, c.f)
				}
				return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					asked = append(asked, req.URL.String())
					if req.URL.Host != "www.cloudflare.com" {
						return nil, errors.New("i/o timeout")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ip=" + c.addr + "\n")), Request: req}, nil
				})}
			}}
			got, err := d.Detect(context.Background(), c.f)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.Addr != c.addr || got.Source != "trace" {
				t.Fatalf("got %+v", got)
			}
			if strings.Join(asked, " ") != strings.Join(c.want, " ") {
				t.Fatalf("asked %q, want %q", asked, c.want)
			}
		})
	}
}

const ifInet6Fixture = `fe800000000000000242acfffe110002 02 40 20 80     eth0
20010db8000000000000000000000002 02 40 00 00     eth0
20010db8000000000000000000000001 02 40 00 00     eth0
20010db80000000000001234abcd0001 02 40 00 01     eth0
20010db8000000000000000000000099 02 40 00 20     eth0
fd000000000000000000000000000001 02 40 00 00     eth0
20010db8000000000000000000000055 03 40 00 00     wlan0
00000000000000000000000000000001 01 80 10 80       lo
`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "if_inet6")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStableIPv6_SkipsTemporaryDeprecatedLinkLocalULA(t *testing.T) {
	got, err := ip.StableIPv6(writeFixture(t, ifInet6Fixture), "eth0")
	if err != nil {
		t.Fatal(err)
	}
	// ::1 and ::2 are both stable; lowest address wins for determinism.
	if got != "2001:db8::1" {
		t.Fatalf("got %q want 2001:db8::1", got)
	}
}

func TestStableIPv6_PrefersPermanent(t *testing.T) {
	fixture := `20010db8000000000000000000000001 02 40 00 00     eth0
20010db8000000000000000000000009 02 40 00 80     eth0
`
	got, err := ip.StableIPv6(writeFixture(t, fixture), "eth0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2001:db8::9" {
		t.Fatalf("got %q want permanent 2001:db8::9", got)
	}
}

func TestStableIPv6_Errors(t *testing.T) {
	p := writeFixture(t, ifInet6Fixture)
	if _, err := ip.StableIPv6(p, "eth9"); err == nil || !strings.Contains(err.Error(), "no IPv6 addresses") {
		t.Errorf("unknown iface: err=%v", err)
	}
	onlyTemp := "20010db80000000000001234abcd0001 02 40 00 01     eth0\n"
	if _, err := ip.StableIPv6(writeFixture(t, onlyTemp), "eth0"); err == nil || !strings.Contains(err.Error(), "no stable") {
		t.Errorf("only temporary: err=%v", err)
	}
	if _, err := ip.StableIPv6(filepath.Join(t.TempDir(), "missing"), "eth0"); err == nil {
		t.Error("missing table: expected error")
	}
	if _, err := ip.StableIPv6(writeFixture(t, "garbage line\n"), "eth0"); err == nil {
		t.Error("garbage: expected error")
	}
}

func TestDetector_V6Iface_UsesInterfaceWithoutNetwork(t *testing.T) {
	d := &ip.Detector{
		V6Iface:     "eth0",
		ProcIfInet6: writeFixture(t, ifInet6Fixture),
		ClientFor: func(ip.Family) *http.Client {
			t.Fatal("trace must not be used when the interface has a stable address")
			return nil
		},
	}
	got, err := d.Detect(context.Background(), ip.IPv6)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != "2001:db8::1" || got.Source != "iface:eth0" {
		t.Fatalf("got %+v", got)
	}
}

func TestDetector_V6Iface_FallsBackToTrace(t *testing.T) {
	d := &ip.Detector{
		V6Iface:     "eth9",
		ProcIfInet6: writeFixture(t, ifInet6Fixture),
		ClientFor:   traceServer(t, "ip=2001:db8:ffff::7\n"),
	}
	got, err := d.Detect(context.Background(), ip.IPv6)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != "2001:db8:ffff::7" {
		t.Fatalf("addr %q", got.Addr)
	}
	if !strings.HasPrefix(got.Source, "trace (fallback, iface eth9:") {
		t.Fatalf("source %q", got.Source)
	}
	// IPv4 never consults the interface table.
	d4 := &ip.Detector{V6Iface: "eth0", ProcIfInet6: writeFixture(t, ifInet6Fixture), ClientFor: traceServer(t, "ip=203.0.113.9\n")}
	got, err = d4.Detect(context.Background(), ip.IPv4)
	if err != nil || got.Addr != "203.0.113.9" || got.Source != "trace" {
		t.Fatalf("v4: %+v %v", got, err)
	}
}

func TestFamily_String(t *testing.T) {
	if ip.IPv4.String() != "ipv4" {
		t.Fatalf("IPv4: %q", ip.IPv4.String())
	}
	if ip.IPv6.String() != "ipv6" {
		t.Fatalf("IPv6: %q", ip.IPv6.String())
	}
}
