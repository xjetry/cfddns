package inspector_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/inspector"
)

type zoneCall struct {
	domain string
}

type findCall struct {
	zoneID string
	typ    string
	name   string
}

type fakeDNS struct {
	token      string
	zoneID     string
	zoneErrors map[string]error
	findErrors map[string]error
	records    map[string][]cloudflare.Record

	zoneCalls   []zoneCall
	findCalls   []findCall
	createCalls int
	updateCalls int
}

func recordKey(typ, name string) string { return typ + "|" + name }

func (f *fakeDNS) ZoneIDFor(_ context.Context, domain string) (string, error) {
	f.zoneCalls = append(f.zoneCalls, zoneCall{domain: domain})
	if err := f.zoneErrors[domain]; err != nil {
		return "", err
	}
	if f.zoneID != "" {
		return f.zoneID, nil
	}
	return "zone-test", nil
}

func (f *fakeDNS) FindRecords(_ context.Context, zoneID, typ, name string) ([]cloudflare.Record, error) {
	f.findCalls = append(f.findCalls, findCall{zoneID: zoneID, typ: typ, name: name})
	if err := f.findErrors[recordKey(typ, name)]; err != nil {
		return nil, err
	}
	return append([]cloudflare.Record(nil), f.records[recordKey(typ, name)]...), nil
}

func (f *fakeDNS) CreateRecord(_ context.Context, _, _, _, _ string) (cloudflare.Record, error) {
	f.createCalls++
	return cloudflare.Record{}, errors.New("unexpected create")
}

func (f *fakeDNS) UpdateRecordContent(_ context.Context, _, _, _ string) error {
	f.updateCalls++
	return errors.New("unexpected update")
}

func TestInspect_FoundRecords(t *testing.T) {
	const (
		domainV4 = "v4.example.com"
		domainV6 = "v6.example.com"
	)
	zone := time.FixedZone("UTC+2", 2*60*60)
	injected := []time.Time{
		time.Date(2026, 10, 27, 12, 30, 1, 123456789, zone),
		time.Date(2026, 10, 27, 12, 30, 5, 987654321, zone),
	}
	nowCalls := 0
	dns := &fakeDNS{records: map[string][]cloudflare.Record{
		recordKey("A", domainV4): {
			{ID: "rec-a", Type: "A", Name: domainV4, Content: "198.51.100.10", Proxied: true, TTL: 300},
		},
		recordKey("AAAA", domainV6): {
			{ID: "rec-aaaa", Type: "AAAA", Name: domainV6, Content: "2001:db8::10", TTL: 1},
		},
	}}
	in := &inspector.Inspector{
		DomainV4: domainV4,
		DomainV6: domainV6,
		DNS:      dns,
		Now: func() time.Time {
			if nowCalls >= len(injected) {
				t.Fatalf("Now called %d times, want %d", nowCalls+1, len(injected))
			}
			now := injected[nowCalls]
			nowCalls++
			return now
		},
	}

	results, allOK := in.Inspect(context.Background())
	if !allOK {
		t.Fatalf("allOK=false, results=%+v", results)
	}
	want := []inspector.Result{
		{
			Family:     "ipv4",
			Domain:     domainV4,
			RecordType: "A",
			Found:      true,
			Content:    "198.51.100.10",
			QueriedAt:  time.Date(2026, 10, 27, 10, 30, 1, 123456789, time.UTC),
		},
		{
			Family:     "ipv6",
			Domain:     domainV6,
			RecordType: "AAAA",
			Found:      true,
			Content:    "2001:db8::10",
			QueriedAt:  time.Date(2026, 10, 27, 10, 30, 5, 987654321, time.UTC),
		},
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results=%+v, want %+v", results, want)
	}
	if nowCalls != 2 {
		t.Fatalf("Now calls=%d, want 2", nowCalls)
	}
	wantFindCalls := []findCall{
		{zoneID: "zone-test", typ: "A", name: domainV4},
		{zoneID: "zone-test", typ: "AAAA", name: domainV6},
	}
	if !reflect.DeepEqual(dns.findCalls, wantFindCalls) {
		t.Fatalf("find calls=%+v, want %+v", dns.findCalls, wantFindCalls)
	}
}

func TestInspect_MissingRecord(t *testing.T) {
	const domain = "missing.example.com"
	queriedAt := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	in := &inspector.Inspector{
		DomainV4: domain,
		DNS:      &fakeDNS{},
		Now:      func() time.Time { return queriedAt },
	}

	results, allOK := in.Inspect(context.Background())
	if !allOK {
		t.Fatalf("a missing record must not fail inspection: %+v", results)
	}
	want := []inspector.Result{{
		Family:     "ipv4",
		Domain:     domain,
		RecordType: "A",
		Found:      false,
		Content:    "",
		QueriedAt:  queriedAt,
		Warning:    "",
		Error:      "",
	}}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results=%+v, want %+v", results, want)
	}
}

func TestInspect_UsesManagedRecord(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: map[string][]cloudflare.Record{
		recordKey("A", domain): {
			{ID: "rec-z", Type: "A", Name: domain, Content: "198.51.100.30"},
			{ID: "rec-a", Type: "A", Name: domain, Content: "198.51.100.10"},
			{ID: "rec-m", Type: "A", Name: domain, Content: "198.51.100.20"},
		},
	}}
	in := &inspector.Inspector{DomainV4: domain, DNS: dns}

	results, allOK := in.Inspect(context.Background())
	if !allOK || len(results) != 1 {
		t.Fatalf("results=%+v allOK=%v", results, allOK)
	}
	result := results[0]
	if !result.Found || result.Content != "198.51.100.10" {
		t.Fatalf("managed record not selected: %+v", result)
	}
	const wantWarning = "3 A records named home.example.com; only rec-a is managed, the other 2 keep their content"
	if result.Warning != wantWarning {
		t.Fatalf("warning=%q, want %q", result.Warning, wantWarning)
	}
}

func TestInspect_PartialFailure(t *testing.T) {
	const (
		domainV4 = "v4.example.com"
		domainV6 = "v6.example.com"
	)
	queryErr := errors.New("cloudflare api: query failed")
	tests := []struct {
		name          string
		zoneError     error
		findError     error
		wantFindCalls int
	}{
		{name: "zone query", zoneError: queryErr, wantFindCalls: 1},
		{name: "record query", findError: queryErr, wantFindCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dns := &fakeDNS{
				zoneErrors: map[string]error{domainV4: tt.zoneError},
				findErrors: map[string]error{recordKey("A", domainV4): tt.findError},
				records: map[string][]cloudflare.Record{
					recordKey("AAAA", domainV6): {
						{ID: "rec-aaaa", Type: "AAAA", Name: domainV6, Content: "2001:db8::20"},
					},
				},
			}
			in := &inspector.Inspector{DomainV4: domainV4, DomainV6: domainV6, DNS: dns}

			results, allOK := in.Inspect(context.Background())
			if allOK {
				t.Fatal("allOK=true, want false for a query error")
			}
			if len(results) != 2 {
				t.Fatalf("results len=%d, want 2: %+v", len(results), results)
			}
			if results[0].Family != "ipv4" || results[0].Error != queryErr.Error() || results[0].Found {
				t.Fatalf("v4 result=%+v", results[0])
			}
			if results[1].Family != "ipv6" || !results[1].Found || results[1].Content != "2001:db8::20" || results[1].Error != "" {
				t.Fatalf("v6 result=%+v", results[1])
			}
			for _, result := range results {
				if result.QueriedAt.IsZero() || result.QueriedAt.Location() != time.UTC {
					t.Fatalf("queried_at=%v, want non-zero UTC", result.QueriedAt)
				}
			}
			if len(dns.findCalls) != tt.wantFindCalls {
				t.Fatalf("find calls=%d, want %d", len(dns.findCalls), tt.wantFindCalls)
			}
		})
	}
}

func TestInspect_DoesNotWrite(t *testing.T) {
	const (
		missingDomain = "missing.example.com"
		foundDomain   = "found.example.com"
	)
	dns := &fakeDNS{records: map[string][]cloudflare.Record{
		recordKey("AAAA", foundDomain): {
			{ID: "rec-aaaa", Type: "AAAA", Name: foundDomain, Content: "2001:db8::30"},
		},
	}}
	in := &inspector.Inspector{DomainV4: missingDomain, DomainV6: foundDomain, DNS: dns}

	results, allOK := in.Inspect(context.Background())
	if !allOK || len(results) != 2 || results[0].Found || !results[1].Found {
		t.Fatalf("results=%+v allOK=%v", results, allOK)
	}
	if dns.createCalls != 0 || dns.updateCalls != 0 {
		t.Fatalf("inspection wrote DNS: creates=%d updates=%d", dns.createCalls, dns.updateCalls)
	}
}

func TestInspect_JSONContainsNoToken(t *testing.T) {
	const (
		domain   = "home.example.com"
		sentinel = "cf-token-sentinel-must-not-leak"
	)
	dns := &fakeDNS{
		token: sentinel,
		records: map[string][]cloudflare.Record{
			recordKey("A", domain): {
				{ID: "rec-a", Type: "A", Name: domain, Content: "198.51.100.40"},
			},
		},
	}
	in := &inspector.Inspector{DomainV4: domain, DNS: dns}

	results, allOK := in.Inspect(context.Background())
	if !allOK {
		t.Fatalf("allOK=false, results=%+v", results)
	}
	payload, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	if strings.Contains(string(payload), sentinel) {
		t.Fatalf("JSON contains configured token: %s", payload)
	}
	if strings.Contains(strings.ToLower(string(payload)), `"token"`) {
		t.Fatalf("JSON contains a token field: %s", payload)
	}
}
