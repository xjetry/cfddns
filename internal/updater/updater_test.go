package updater_test

import (
	"context"
	"errors"
	"testing"

	"cfddns/internal/cloudflare"
	"cfddns/internal/ip"
	"cfddns/internal/updater"
)

type fakeIP struct {
	v4    string
	v4Err error
	v6    string
	v6Err error
}

func (f *fakeIP) Detect(_ context.Context, fam ip.Family) (string, error) {
	if fam == ip.IPv6 {
		return f.v6, f.v6Err
	}
	return f.v4, f.v4Err
}

type fakeDNS struct {
	zoneID string
	zoneErr error

	// key: type+"|"+name
	records map[string]*cloudflare.Record

	creates []cloudflare.Record
	updates []updateCall
}

type updateCall struct {
	zoneID string
	id     string
	rec    cloudflare.Record
}

func (d *fakeDNS) ZoneIDFor(_ context.Context, _ string) (string, error) {
	if d.zoneErr != nil {
		return "", d.zoneErr
	}
	if d.zoneID == "" {
		return "zone1", nil
	}
	return d.zoneID, nil
}

func (d *fakeDNS) FindRecord(_ context.Context, _, typ, name string) (*cloudflare.Record, error) {
	if d.records == nil {
		return nil, nil
	}
	return d.records[typ+"|"+name], nil
}

func (d *fakeDNS) CreateRecord(_ context.Context, _ string, rec cloudflare.Record) error {
	d.creates = append(d.creates, rec)
	return nil
}

func (d *fakeDNS) UpdateRecord(_ context.Context, zoneID, id string, rec cloudflare.Record) error {
	d.updates = append(d.updates, updateCall{zoneID: zoneID, id: id, rec: rec})
	return nil
}

func TestRun_IPv6DetectFails_StillUpdatesV4(t *testing.T) {
	dns := &fakeDNS{records: map[string]*cloudflare.Record{}}
	up := &updater.Updater{
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		IP: &fakeIP{
			v4:    "203.0.113.10",
			v6Err: errors.New("no route to host"),
		},
		DNS: dns,
	}

	results, allOK := up.Run(context.Background())
	if allOK {
		t.Fatal("expected allOK false when IPv6 detect fails")
	}
	if len(results) != 2 {
		t.Fatalf("results len=%d want 2", len(results))
	}

	if results[0].Family != "ipv4" || results[0].Action != "created" {
		t.Fatalf("v4 result: %+v", results[0])
	}
	if results[0].IP != "203.0.113.10" || results[0].Domain != "v4.example.com" {
		t.Fatalf("v4 result: %+v", results[0])
	}

	if results[1].Family != "ipv6" || results[1].Action != "error" {
		t.Fatalf("v6 result: %+v", results[1])
	}
	if results[1].Error == "" {
		t.Fatal("v6 error message empty")
	}
	if results[1].IP != "" {
		t.Fatalf("v6 should not set IP on detect failure: %+v", results[1])
	}

	if len(dns.creates) != 1 {
		t.Fatalf("creates=%d want 1 (A only)", len(dns.creates))
	}
	if dns.creates[0].Type != "A" || dns.creates[0].Content != "203.0.113.10" {
		t.Fatalf("create: %+v", dns.creates[0])
	}
	if len(dns.updates) != 0 {
		t.Fatalf("unexpected updates: %+v", dns.updates)
	}
}

func TestRun_UnchangedSkipsUpdate(t *testing.T) {
	const domain = "home.example.com"
	const addr = "198.51.100.1"
	dns := &fakeDNS{
		records: map[string]*cloudflare.Record{
			"A|" + domain: {
				ID:      "rec-a",
				Type:    "A",
				Name:    domain,
				Content: addr,
			},
		},
	}
	up := &updater.Updater{
		DomainV4: domain,
		IP:       &fakeIP{v4: addr},
		DNS:      dns,
	}

	results, allOK := up.Run(context.Background())
	if !allOK {
		t.Fatal("expected allOK true")
	}
	if len(results) != 1 {
		t.Fatalf("results len=%d", len(results))
	}
	if results[0].Action != "unchanged" || results[0].IP != addr {
		t.Fatalf("result: %+v", results[0])
	}
	if len(dns.creates) != 0 || len(dns.updates) != 0 {
		t.Fatalf("creates=%d updates=%d want none", len(dns.creates), len(dns.updates))
	}
}

func TestRun_CreateWhenMissing(t *testing.T) {
	const domain = "home.example.com"
	const addr = "198.51.100.50"
	dns := &fakeDNS{records: map[string]*cloudflare.Record{}}
	up := &updater.Updater{
		DomainV4: domain,
		IP:       &fakeIP{v4: addr},
		DNS:      dns,
	}

	results, allOK := up.Run(context.Background())
	if !allOK {
		t.Fatal("expected allOK true")
	}
	if len(results) != 1 || results[0].Action != "created" {
		t.Fatalf("results: %+v", results)
	}
	if len(dns.creates) != 1 {
		t.Fatalf("creates=%d", len(dns.creates))
	}
	c := dns.creates[0]
	if c.Type != "A" || c.Name != domain || c.Content != addr {
		t.Fatalf("create record: %+v", c)
	}
	if c.Proxied {
		t.Fatal("proxied should be false")
	}
	if c.TTL != 1 {
		t.Fatalf("ttl=%d want 1", c.TTL)
	}
	if len(dns.updates) != 0 {
		t.Fatalf("unexpected updates: %+v", dns.updates)
	}
}
