package cloudflare

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client talks to the Cloudflare API v4 for zone and DNS record operations.
type Client struct {
	Token      string
	HTTPClient *http.Client
	BaseURL    string // default https://api.cloudflare.com/client/v4

	once      sync.Once
	transport *http.Transport
	client    *http.Client
}

// Record is a DNS record as read from the API.
type Record struct {
	ID      string
	Type    string
	Name    string
	Content string
	Proxied bool
	TTL     int
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []apiError      `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *resultInfo     `json:"result_info"`
}

type resultInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	TotalPages int `json:"total_pages"`
	Count      int `json:"count"`
	TotalCount int `json:"total_count"`
}

type zoneResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type recordResult struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func (r recordResult) record() Record { return Record(r) }

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

// httpClient returns the client built once for this Client. It is not the
// default transport: HTTP/2 is disabled because an h2 connection carries
// 64 KB framer and HPACK buffers that a few small JSON calls per hour never
// need, and the idle pool is kept to the one or two connections a cycle can
// use so nothing lingers on hosts with little memory.
func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.once.Do(func() {
		c.transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// A non-nil TLSNextProto map tells the transport the caller
			// configured protocols, so it does not register h2 itself.
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
			MaxIdleConns:        2,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		// Bound waits so a hung API call does not hold the updater lock forever.
		c.client = &http.Client{Timeout: 30 * time.Second, Transport: c.transport}
	})
	return c.client
}

// CloseIdleConnections drops pooled API connections; the daemon calls it
// after every cycle so a connection never idles across a period.
func (c *Client) CloseIdleConnections() {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// ZoneIDFor lists accessible zones (paginated) and returns the ID of the
// longest zone name that equals fqdn or is a DNS suffix of fqdn.
func (c *Client) ZoneIDFor(ctx context.Context, fqdn string) (string, error) {
	fqdn = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
	if fqdn == "" {
		return "", fmt.Errorf("empty fqdn")
	}

	var best zoneResult
	page := 1
	for {
		q := url.Values{}
		q.Set("per_page", "50")
		q.Set("page", strconv.Itoa(page))
		path := "/zones?" + q.Encode()

		var zones []zoneResult
		info, err := c.do(ctx, http.MethodGet, path, nil, &zones)
		if err != nil {
			return "", err
		}
		for _, z := range zones {
			name := strings.ToLower(strings.TrimSuffix(z.Name, "."))
			if !isZoneMatch(fqdn, name) {
				continue
			}
			if best.ID == "" || len(name) > len(best.Name) {
				best = zoneResult{ID: z.ID, Name: name}
			}
		}
		if info == nil || page >= info.TotalPages || len(zones) == 0 {
			break
		}
		page++
	}
	if best.ID == "" {
		return "", fmt.Errorf("no zone found for %q", fqdn)
	}
	return best.ID, nil
}

// isZoneMatch reports whether zone is equal to fqdn or a proper DNS suffix of it.
func isZoneMatch(fqdn, zone string) bool {
	if fqdn == zone {
		return true
	}
	return strings.HasSuffix(fqdn, "."+zone)
}

// FindRecords returns every DNS record in the zone matching type and name,
// in API order. An empty slice means the record does not exist.
func (c *Client) FindRecords(ctx context.Context, zoneID, typ, name string) ([]Record, error) {
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", name)
	q.Set("per_page", "100")
	path := fmt.Sprintf("/zones/%s/dns_records?%s", url.PathEscape(zoneID), q.Encode())

	var results []recordResult
	if _, err := c.do(ctx, http.MethodGet, path, nil, &results); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(results))
	for _, r := range results {
		records = append(records, r.record())
	}
	return records, nil
}

// CreateRecord creates a DNS-only record (proxied=false, ttl=1 automatic) and
// returns it with the ID assigned by Cloudflare.
func (c *Client) CreateRecord(ctx context.Context, zoneID, typ, name, content string) (Record, error) {
	body := struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
		TTL     int    `json:"ttl"`
	}{Type: typ, Name: name, Content: content, Proxied: false, TTL: 1}
	path := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(zoneID))
	var created recordResult
	if _, err := c.do(ctx, http.MethodPost, path, body, &created); err != nil {
		return Record{}, err
	}
	if created.ID == "" {
		return Record{}, fmt.Errorf("cloudflare api: create returned no record id")
	}
	return created.record(), nil
}

// UpdateRecordContent changes only the content of an existing record. PATCH
// (not PUT) is used so fields managed in the dashboard — proxied, ttl,
// comment, tags — are left exactly as the operator set them.
func (c *Client) UpdateRecordContent(ctx context.Context, zoneID, recordID, content string) error {
	body := struct {
		Content string `json:"content"`
	}{Content: content}
	path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(recordID))
	_, err := c.do(ctx, http.MethodPatch, path, body, nil)
	return err
}

// do performs an authenticated API request and decodes result into dest when non-nil.
// Returns pagination info when present.
func (c *Client) do(ctx context.Context, method, path string, body any, dest any) (*resultInfo, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}

	u := c.baseURL() + path
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare api: status %d: invalid json: %w", resp.StatusCode, err)
	}
	if !env.Success {
		return nil, fmt.Errorf("cloudflare api: %s", formatAPIErrors(env.Errors))
	}
	if dest != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, dest); err != nil {
			return nil, fmt.Errorf("cloudflare api: decode result: %w", err)
		}
	}
	return env.ResultInfo, nil
}

func formatAPIErrors(errs []apiError) string {
	if len(errs) == 0 {
		return "request failed"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		if e.Message != "" {
			parts = append(parts, e.Message)
		}
	}
	if len(parts) == 0 {
		return "request failed"
	}
	return strings.Join(parts, "; ")
}
