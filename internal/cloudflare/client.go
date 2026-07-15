package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client talks to the Cloudflare API v4 for zone and DNS record operations.
type Client struct {
	Token      string
	HTTPClient *http.Client
	BaseURL    string // default https://api.cloudflare.com/client/v4
}

// Record is a DNS record used for list/create/update.
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

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
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

// FindRecord returns the first DNS record matching type and name, or nil, nil if none.
func (c *Client) FindRecord(ctx context.Context, zoneID, typ, name string) (*Record, error) {
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", name)
	path := fmt.Sprintf("/zones/%s/dns_records?%s", url.PathEscape(zoneID), q.Encode())

	var records []recordResult
	if _, err := c.do(ctx, http.MethodGet, path, nil, &records); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	r := records[0]
	return &Record{
		ID:      r.ID,
		Type:    r.Type,
		Name:    r.Name,
		Content: r.Content,
		Proxied: r.Proxied,
		TTL:     r.TTL,
	}, nil
}

// CreateRecord creates a DNS record. proxied is forced to false and ttl to 1 (automatic).
func (c *Client) CreateRecord(ctx context.Context, zoneID string, rec Record) error {
	body := recordBody{
		Type:    rec.Type,
		Name:    rec.Name,
		Content: rec.Content,
		Proxied: false,
		TTL:     1,
	}
	path := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(zoneID))
	_, err := c.do(ctx, http.MethodPost, path, body, nil)
	return err
}

// UpdateRecord replaces a DNS record. proxied is forced to false and ttl to 1 (automatic).
func (c *Client) UpdateRecord(ctx context.Context, zoneID, recordID string, rec Record) error {
	body := recordBody{
		Type:    rec.Type,
		Name:    rec.Name,
		Content: rec.Content,
		Proxied: false,
		TTL:     1,
	}
	path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(recordID))
	_, err := c.do(ctx, http.MethodPut, path, body, nil)
	return err
}

type recordBody struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
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
