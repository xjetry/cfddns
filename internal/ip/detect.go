package ip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// defaultTraceURLs lists each family's trace endpoints in the order they are
// tried. 1.1.1.1 is unreachable on some networks; www.cloudflare.com serves
// the same trace, and the family's forced dial keeps the answer in-family.
var defaultTraceURLs = [2][]string{
	IPv4: {"https://1.1.1.1/cdn-cgi/trace", "https://www.cloudflare.com/cdn-cgi/trace"},
	IPv6: {"https://[2606:4700:4700::1111]/cdn-cgi/trace", "https://www.cloudflare.com/cdn-cgi/trace"},
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
	// traceURLs overrides defaultTraceURLs per family (tests).
	traceURLs [2][]string

	once    sync.Once
	clients [2]*http.Client
	// lastTrace is the index of the trace endpoint that last answered, per
	// family; it is asked first next time.
	lastTrace [2]atomic.Int32
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

// CloseIdleConnections drops the keep-alive connections of both family
// clients. A connection kept across a period would hit IdleConnTimeout
// before the next cycle anyway; closing it right after the cycle frees its
// TLS buffers and reader goroutines immediately on hosts with little memory.
func (d *Detector) CloseIdleConnections() {
	for _, c := range d.clients {
		if c == nil {
			continue
		}
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
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

func (d *Detector) traceEndpoints(f Family) []string {
	if len(d.traceURLs[f]) > 0 {
		return d.traceURLs[f]
	}
	return defaultTraceURLs[f]
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

// trace asks the trace endpoints of f in turn, starting with the one that
// answered last time, and stops at the first in-family answer or once ctx is
// done. The error names every endpoint asked and why it failed.
func (d *Detector) trace(ctx context.Context, f Family) (string, error) {
	urls := d.traceEndpoints(f)
	first := int(d.lastTrace[f].Load())
	if first >= len(urls) {
		first = 0
	}
	order := []int{first}
	for i := range urls {
		if i != first {
			order = append(order, i)
		}
	}
	var errs []string
	for _, i := range order {
		if ctx.Err() != nil {
			break
		}
		addr, err := d.traceURL(ctx, f, urls[i])
		if err == nil {
			d.lastTrace[f].Store(int32(i))
			return addr, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", urls[i], err))
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("trace %s: %w", f, ctx.Err())
	}
	return "", fmt.Errorf("trace %s: %s", f, strings.Join(errs, "; "))
}

func (d *Detector) traceURL(ctx context.Context, f Family, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := d.client(f).Do(req)
	if err != nil {
		// Missing stack / no route surfaces here — caller treats as soft error.
		// trace already names the endpoint, so drop the URL wrapper.
		var ue *url.Error
		if errors.As(err, &ue) {
			return "", ue.Err
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	addr, err := ParseTraceIP(string(b))
	if err != nil {
		return "", err
	}
	if err := checkFamily(addr, f); err != nil {
		return "", err
	}
	return addr, nil
}

// checkFamily rejects a trace answer outside f: IPv4 must be a 4-byte
// address, IPv6 must be neither IPv4 nor IPv4-mapped.
func checkFamily(addr string, f Family) error {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return err
	}
	if f == IPv4 && a.Is4() || f == IPv6 && a.Is6() && !a.Is4In6() {
		return nil
	}
	return fmt.Errorf("answer %s is not an %s address", addr, f)
}
