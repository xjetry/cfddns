package ip

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
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

// Detection is a detected public address and where it came from.
type Detection struct {
	Addr string
	// Source is "trace", "iface:<name>", or "trace (fallback, iface <name>: <reason>)"
	// when an interface lookup was configured but yielded nothing.
	Source string
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

// Detector fetches the public IP via Cloudflare trace with a forced address
// family, optionally preferring a stable interface address for IPv6.
type Detector struct {
	Timeout time.Duration
	// V6Iface, when set, names the interface whose stable global IPv6 address
	// is used instead of the trace answer (see StableIPv6).
	V6Iface string
	// ProcIfInet6 overrides the kernel IPv6 address table path (tests).
	ProcIfInet6 string
	// ClientFor optional override for tests: func(Family) *http.Client
	ClientFor func(Family) *http.Client

	once    sync.Once
	clients [2]*http.Client
}

// client returns the long-lived client for f. One Transport per family is
// built once and reused for the life of the Detector: a Transport owns its
// keep-alive connections and their reader goroutines, so building one per
// call would leak a connection and two goroutines every cycle until the
// remote side closes them.
func (d *Detector) client(f Family) *http.Client {
	if d.ClientFor != nil {
		return d.ClientFor(f)
	}
	d.once.Do(func() {
		d.clients[IPv4] = d.newClient(IPv4)
		d.clients[IPv6] = d.newClient(IPv6)
	})
	return d.clients[f]
}

func (d *Detector) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 10 * time.Second
}

func (d *Detector) newClient(f Family) *http.Client {
	timeout := d.timeout()
	network := "tcp4"
	if f == IPv6 {
		network = "tcp6"
	}
	tr := &http.Transport{
		// No Proxy on purpose: the trace answer is the address the server
		// sees, so going through a proxy would report the proxy's egress
		// instead of this host's own address.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: timeout,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func (d *Detector) procPath() string {
	if d.ProcIfInet6 != "" {
		return d.ProcIfInet6
	}
	return DefaultProcIfInet6
}

// Detect returns the public IP for the given address family. For IPv6 with
// V6Iface set, the interface's stable address wins; when none is usable the
// trace path runs and the returned Source records why.
// Dial/timeout errors are returned to the caller (soft failure); never panics.
func (d *Detector) Detect(ctx context.Context, f Family) (Detection, error) {
	source := "trace"
	if f == IPv6 && d.V6Iface != "" {
		addr, err := StableIPv6(d.procPath(), d.V6Iface)
		if err == nil {
			return Detection{Addr: addr, Source: "iface:" + d.V6Iface}, nil
		}
		source = fmt.Sprintf("trace (fallback, iface %s: %v)", d.V6Iface, err)
	}
	addr, err := d.trace(ctx, f)
	if err != nil {
		return Detection{}, err
	}
	return Detection{Addr: addr, Source: source}, nil
}

func (d *Detector) trace(ctx context.Context, f Family) (string, error) {
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
