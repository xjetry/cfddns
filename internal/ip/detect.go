package ip

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Family int

const (
	IPv4 Family = iota
	IPv6
)

func (f Family) String() string {
	if f == IPv6 {
		return "ipv6"
	}
	return "ipv4"
}

// ParseTraceIP extracts and validates the ip= field from a Cloudflare trace body.
func ParseTraceIP(body string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ip=") {
			v := strings.TrimPrefix(line, "ip=")
			if v == "" {
				return "", fmt.Errorf("empty ip field")
			}
			if net.ParseIP(v) == nil {
				return "", fmt.Errorf("invalid ip %q", v)
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("ip= not found in trace")
}

// Detector fetches public IP via Cloudflare trace with forced address family.
type Detector struct {
	Timeout time.Duration
	// ClientFor optional override for tests: func(Family) *http.Client
	ClientFor func(Family) *http.Client
}

func (d *Detector) client(f Family) *http.Client {
	if d.ClientFor != nil {
		return d.ClientFor(f)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	network := "tcp4"
	if f == IPv6 {
		network = "tcp6"
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// Detect returns the public IP for the given address family by querying
// Cloudflare's /cdn-cgi/trace endpoint over a family-forced dialer.
// Dial/timeout errors are returned to the caller (soft failure); never panics.
func (d *Detector) Detect(ctx context.Context, f Family) (string, error) {
	url := "https://1.1.1.1/cdn-cgi/trace"
	if f == IPv6 {
		url = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := d.client(f).Do(req)
	if err != nil {
		// Missing stack / no route surfaces here — caller treats as soft error.
		return "", fmt.Errorf("trace %s: %w", f, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("trace %s: status %d", f, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return ParseTraceIP(string(b))
}
