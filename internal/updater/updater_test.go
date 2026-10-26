package updater_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/ip"
	"cfddns/internal/updater"
)

type fakeIP struct {
	v4    string
	v4Err error
	v6    string
	v6Err error
	// block, when non-nil, makes Detect wait until it is closed.
	block chan struct{}
}

func (f *fakeIP) Detect(ctx context.Context, fam ip.Family) (ip.Detection, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ip.Detection{}, ctx.Err()
		}
	}
	if fam == ip.IPv6 {
		return ip.Detection{Addr: f.v6, Source: "iface:eth0"}, f.v6Err
	}
	return ip.Detection{Addr: f.v4, Source: "trace"}, f.v4Err
}

type updateCall struct {
	zoneID  string
	id      string
	content string
}

type fakeDNS struct {
	zoneID  string
	zoneErr error
	findErr error

	// key: type+"|"+name
	records map[string][]cloudflare.Record

	// updateErrs are consumed one per UpdateRecordContent call.
	updateErrs []error
	nextID     int

	zoneCalls int
	findCalls int
	creates   []cloudflare.Record
	updates   []updateCall
}

func (d *fakeDNS) ZoneIDFor(_ context.Context, _ string) (string, error) {
	d.zoneCalls++
	if d.zoneErr != nil {
		return "", d.zoneErr
	}
	if d.zoneID == "" {
		return "zone1", nil
	}
	return d.zoneID, nil
}

func (d *fakeDNS) FindRecords(_ context.Context, _, typ, name string) ([]cloudflare.Record, error) {
	d.findCalls++
	if d.findErr != nil {
		return nil, d.findErr
	}
	out := append([]cloudflare.Record(nil), d.records[typ+"|"+name]...)
	return out, nil
}

func (d *fakeDNS) CreateRecord(_ context.Context, _ string, typ, name, content string) (cloudflare.Record, error) {
	d.nextID++
	rec := cloudflare.Record{ID: "new" + string(rune('0'+d.nextID)), Type: typ, Name: name, Content: content}
	d.creates = append(d.creates, rec)
	if d.records == nil {
		d.records = map[string][]cloudflare.Record{}
	}
	d.records[typ+"|"+name] = append(d.records[typ+"|"+name], rec)
	return rec, nil
}

func (d *fakeDNS) UpdateRecordContent(_ context.Context, zoneID, id, content string) error {
	d.updates = append(d.updates, updateCall{zoneID: zoneID, id: id, content: content})
	if len(d.updateErrs) > 0 {
		err := d.updateErrs[0]
		d.updateErrs = d.updateErrs[1:]
		if err != nil {
			return err
		}
	}
	for k, recs := range d.records {
		for i := range recs {
			if recs[i].ID == id {
				d.records[k][i].Content = content
			}
		}
	}
	return nil
}

func oneRecord(domain, id, typ, content string) map[string][]cloudflare.Record {
	return map[string][]cloudflare.Record{typ + "|" + domain: {{ID: id, Type: typ, Name: domain, Content: content}}}
}

func TestRun_IPv6DetectFails_StillUpdatesV4(t *testing.T) {
	dns := &fakeDNS{}
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

	if results[0].Family != "ipv4" || results[0].Action != updater.ActionCreated {
		t.Fatalf("v4 result: %+v", results[0])
	}
	if results[0].IP != "203.0.113.10" || results[0].Domain != "v4.example.com" || results[0].Source != "trace" {
		t.Fatalf("v4 result: %+v", results[0])
	}

	if results[1].Family != "ipv6" || results[1].Action != updater.ActionError {
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
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", addr)}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: addr}, DNS: dns}

	results, allOK := up.Run(context.Background())
	if !allOK {
		t.Fatal("expected allOK true")
	}
	if len(results) != 1 {
		t.Fatalf("results len=%d", len(results))
	}
	if results[0].Action != updater.ActionUnchanged || results[0].IP != addr {
		t.Fatalf("result: %+v", results[0])
	}
	if len(dns.creates) != 0 || len(dns.updates) != 0 {
		t.Fatalf("creates=%d updates=%d want none", len(dns.creates), len(dns.updates))
	}
}

func TestRun_CreateWhenMissing(t *testing.T) {
	const domain = "home.example.com"
	const addr = "198.51.100.50"
	dns := &fakeDNS{}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: addr}, DNS: dns}

	results, allOK := up.Run(context.Background())
	if !allOK {
		t.Fatal("expected allOK true")
	}
	if len(results) != 1 || results[0].Action != updater.ActionCreated {
		t.Fatalf("results: %+v", results)
	}
	if len(dns.creates) != 1 {
		t.Fatalf("creates=%d", len(dns.creates))
	}
	c := dns.creates[0]
	if c.Type != "A" || c.Name != domain || c.Content != addr {
		t.Fatalf("create record: %+v", c)
	}
	if len(dns.updates) != 0 {
		t.Fatalf("unexpected updates: %+v", dns.updates)
	}
}

func TestRun_UpdatesExistingRecord(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{zoneID: "zone-x", records: oneRecord(domain, "rec-old", "AAAA", "2001:db8::1")}
	up := &updater.Updater{DomainV6: domain, IP: &fakeIP{v6: "2001:db8::2"}, DNS: dns}

	results, allOK := up.Run(context.Background())
	if !allOK || len(results) != 1 || results[0].Action != updater.ActionUpdated {
		t.Fatalf("results=%+v allOK=%v", results, allOK)
	}
	if results[0].Source != "iface:eth0" {
		t.Fatalf("source not propagated: %+v", results[0])
	}
	if len(dns.updates) != 1 || dns.updates[0] != (updateCall{zoneID: "zone-x", id: "rec-old", content: "2001:db8::2"}) {
		t.Fatalf("updates: %+v", dns.updates)
	}
	if len(dns.creates) != 0 {
		t.Fatalf("unexpected creates: %+v", dns.creates)
	}
}

func TestRun_CacheServesUnchangedWithoutAPI(t *testing.T) {
	const domain = "home.example.com"
	const addr = "198.51.100.1"
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", addr)}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: addr}, DNS: dns}

	up.Run(context.Background())
	if dns.zoneCalls != 1 || dns.findCalls != 1 {
		t.Fatalf("first cycle: zone=%d find=%d", dns.zoneCalls, dns.findCalls)
	}
	for i := 0; i < updater.RecheckEvery; i++ {
		results, ok := up.Run(context.Background())
		if !ok || results[0].Action != updater.ActionUnchanged {
			t.Fatalf("cycle %d: %+v", i+2, results)
		}
	}
	if dns.zoneCalls != 1 || dns.findCalls != 1 {
		t.Fatalf("cached cycles must not call the API: zone=%d find=%d", dns.zoneCalls, dns.findCalls)
	}

	// The cycle after RecheckEvery cached ones re-reads the record.
	up.Run(context.Background())
	if dns.findCalls != 2 {
		t.Fatalf("recheck expected after %d cached cycles: find=%d", updater.RecheckEvery, dns.findCalls)
	}
}

func TestRun_CacheUpdatesViaPatchWithoutLookup(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{zoneID: "zone-x", records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	det := &fakeIP{v4: "198.51.100.1"}
	up := &updater.Updater{DomainV4: domain, IP: det, DNS: dns}

	up.Run(context.Background())
	det.v4 = "198.51.100.2"
	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUpdated {
		t.Fatalf("results=%+v", results)
	}
	if dns.zoneCalls != 1 || dns.findCalls != 1 {
		t.Fatalf("IP change must patch the cached record without lookups: zone=%d find=%d", dns.zoneCalls, dns.findCalls)
	}
	if len(dns.updates) != 1 || dns.updates[0] != (updateCall{zoneID: "zone-x", id: "rec-a", content: "198.51.100.2"}) {
		t.Fatalf("updates: %+v", dns.updates)
	}
	// Then the new value is served from cache.
	results, _ = up.Run(context.Background())
	if results[0].Action != updater.ActionUnchanged || dns.findCalls != 1 {
		t.Fatalf("after patch: %+v find=%d", results, dns.findCalls)
	}
}

func TestRun_StaleCacheFallsBackToFullResolve(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	det := &fakeIP{v4: "198.51.100.1"}
	up := &updater.Updater{DomainV4: domain, IP: det, DNS: dns}
	up.Run(context.Background())

	// Simulate the record having been recreated in the dashboard: the cached
	// ID no longer exists, the first PATCH fails.
	dns.records = oneRecord(domain, "rec-b", "A", "198.51.100.1")
	dns.updateErrs = []error{errors.New("cloudflare api: record not found")}
	det.v4 = "198.51.100.2"

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUpdated || results[0].Error != "" {
		t.Fatalf("results=%+v", results)
	}
	if dns.findCalls != 2 {
		t.Fatalf("expected a full resolve after the stale PATCH: find=%d", dns.findCalls)
	}
	if len(dns.updates) != 2 || dns.updates[1].id != "rec-b" || dns.updates[1].content != "198.51.100.2" {
		t.Fatalf("updates: %+v", dns.updates)
	}
}

func TestRun_RecheckCorrectsDashboardEdit(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}
	up.Run(context.Background())

	dns.records["A|"+domain][0].Content = "203.0.113.99" // edited behind our back
	for i := 0; i < updater.RecheckEvery; i++ {
		if r, _ := up.Run(context.Background()); r[0].Action != updater.ActionUnchanged {
			t.Fatalf("cached cycle %d: %+v", i, r)
		}
	}
	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUpdated {
		t.Fatalf("recheck must correct the edit: %+v", results)
	}
	if len(dns.updates) != 1 || dns.updates[0].content != "198.51.100.1" {
		t.Fatalf("updates: %+v", dns.updates)
	}
}

func TestRun_MultipleRecordsWarnsAndManagesLowestID(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: map[string][]cloudflare.Record{
		"A|" + domain: {
			{ID: "rec-z", Type: "A", Name: domain, Content: "198.51.100.1"},
			{ID: "rec-a", Type: "A", Name: domain, Content: "198.51.100.1"},
			{ID: "rec-m", Type: "A", Name: domain, Content: "198.51.100.1"},
		},
	}}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.2"}, DNS: dns}

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUpdated {
		t.Fatalf("results=%+v", results)
	}
	w := results[0].Warning
	if !strings.Contains(w, "3 A records") || !strings.Contains(w, "rec-a") || !strings.Contains(w, "other 2") {
		t.Fatalf("warning = %q", w)
	}
	if len(dns.updates) != 1 || dns.updates[0].id != "rec-a" {
		t.Fatalf("must manage the lowest ID only: %+v", dns.updates)
	}
	// Single record: no warning.
	dns2 := &fakeDNS{records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	up2 := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns2}
	if r, _ := up2.Run(context.Background()); r[0].Warning != "" {
		t.Fatalf("unexpected warning: %q", r[0].Warning)
	}
}

func TestRun_ErrorsAreNotCached(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{zoneErr: errors.New("cloudflare api: Authentication error")}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}

	for i := 0; i < 2; i++ {
		results, ok := up.Run(context.Background())
		if ok || results[0].Action != updater.ActionError || !strings.Contains(results[0].Error, "Authentication error") {
			t.Fatalf("cycle %d: %+v", i, results)
		}
	}
	if dns.zoneCalls != 2 {
		t.Fatalf("a failed cycle must retry from scratch: zone=%d", dns.zoneCalls)
	}

	dns.zoneErr = nil
	dns.findErr = errors.New("cloudflare api: boom")
	if results, _ := up.Run(context.Background()); results[0].Action != updater.ActionError {
		t.Fatalf("find error: %+v", results)
	}
	dns.findErr = nil
	dns.updateErrs = []error{errors.New("cloudflare api: patch failed")}
	dns.records = oneRecord(domain, "rec-a", "A", "198.51.100.9")
	if results, _ := up.Run(context.Background()); results[0].Action != updater.ActionError || !strings.Contains(results[0].Error, "patch failed") {
		t.Fatalf("update error on resolve path must surface: %+v", results)
	}
}

func TestRun_WaitingForLockRespectsContext(t *testing.T) {
	const domain = "home.example.com"
	block := make(chan struct{})
	det := &fakeIP{v4: "198.51.100.1", block: block}
	up := &updater.Updater{DomainV4: domain, DomainV6: domain, IP: det, DNS: &fakeDNS{}}

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		up.Run(context.Background())
		close(done)
	}()
	<-started
	// Give the goroutine time to take the lock and block in Detect.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	results, ok := up.Run(ctx)
	if ok || len(results) != 2 {
		t.Fatalf("results=%+v ok=%v", results, ok)
	}
	for _, r := range results {
		if r.Action != updater.ActionError || !strings.Contains(r.Error, "waiting for a running update cycle") {
			t.Fatalf("result %+v", r)
		}
	}
	if time.Since(t0) > time.Second {
		t.Fatal("waiter did not give up at its deadline")
	}
	close(block)
	<-done
}
