package updater_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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

	// updateErrs are consumed one per UpdateRecordContent call; findErrs one
	// per FindRecords call (a nil entry answers normally).
	updateErrs []error
	findErrs   []error
	nextID     int
	// afterCreate runs once the created record is stored, before it is
	// returned: the hook stands in for another process acting on the zone
	// in the same instant.
	afterCreate func()

	zoneCalls int
	findCalls int
	creates   []cloudflare.Record
	updates   []updateCall
	deletes   []string
	deleteErr error
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
	if len(d.findErrs) > 0 {
		err := d.findErrs[0]
		d.findErrs = d.findErrs[1:]
		if err != nil {
			return nil, err
		}
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
	if d.afterCreate != nil {
		d.afterCreate()
	}
	return rec, nil
}

func (d *fakeDNS) DeleteRecord(_ context.Context, _, id string) error {
	d.deletes = append(d.deletes, id)
	if d.deleteErr != nil {
		return d.deleteErr
	}
	for k, recs := range d.records {
		kept := recs[:0]
		for _, r := range recs {
			if r.ID != id {
				kept = append(kept, r)
			}
		}
		d.records[k] = kept
	}
	return nil
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
	dns.updateErrs = []error{&cloudflare.APIError{Status: 404}}
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
		if r.DetectedAt != nil || r.FinishedAt == nil {
			t.Fatalf("lock-wait timestamps: DetectedAt=%v FinishedAt=%v", r.DetectedAt, r.FinishedAt)
		}
		if r.FinishedAt.Location() != time.UTC {
			t.Fatalf("FinishedAt location = %v, want UTC", r.FinishedAt.Location())
		}
	}
	if time.Since(t0) > time.Second {
		t.Fatal("waiter did not give up at its deadline")
	}
	close(block)
	<-done
}

func TestRun_CachedPatchTransientErrorKeepsCache(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	det := &fakeIP{v4: "198.51.100.1"}
	up := &updater.Updater{DomainV4: domain, IP: det, DNS: dns}
	up.Run(context.Background())

	// Rate limited (429) and a transport failure: the record is still there,
	// so the cycle must report the error and leave the cached IDs for the
	// next cycle instead of spending three more calls on a resolve.
	det.v4 = "198.51.100.2"
	dns.updateErrs = []error{&cloudflare.APIError{Status: 429}, errors.New("dial tcp: i/o timeout")}
	for i := 0; i < 2; i++ {
		results, ok := up.Run(context.Background())
		if ok || results[0].Action != updater.ActionError {
			t.Fatalf("cycle %d: %+v", i, results)
		}
	}
	if dns.zoneCalls != 1 || dns.findCalls != 1 {
		t.Fatalf("a transient PATCH failure must not trigger a resolve: zone=%d find=%d", dns.zoneCalls, dns.findCalls)
	}
	if len(dns.updates) != 2 {
		t.Fatalf("every cycle must retry the PATCH: %+v", dns.updates)
	}

	// Once the API answers again the cached record is patched without lookups.
	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUpdated || dns.findCalls != 1 {
		t.Fatalf("recovery: %+v find=%d", results, dns.findCalls)
	}
	if last := dns.updates[len(dns.updates)-1]; last.id != "rec-a" || last.content != "198.51.100.2" {
		t.Fatalf("updates: %+v", dns.updates)
	}
}

func TestRun_CachedCyclesKeepDuplicateWarning(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: map[string][]cloudflare.Record{
		"A|" + domain: {
			{ID: "rec-a", Type: "A", Name: domain, Content: "198.51.100.1"},
			{ID: "rec-b", Type: "A", Name: domain, Content: "198.51.100.1"},
		},
	}}
	det := &fakeIP{v4: "198.51.100.1"}
	up := &updater.Updater{DomainV4: domain, IP: det, DNS: dns}

	first, _ := up.Run(context.Background())
	if !strings.Contains(first[0].Warning, "2 A records") {
		t.Fatalf("first: %+v", first)
	}
	// Unchanged from cache, then a PATCH from cache: both are answered
	// without a lookup, and both must still say the duplicate exists.
	cached, _ := up.Run(context.Background())
	det.v4 = "198.51.100.2"
	patched, _ := up.Run(context.Background())
	for _, r := range []updater.Result{cached[0], patched[0]} {
		if r.Warning != first[0].Warning {
			t.Fatalf("cached cycle lost the warning: %+v", r)
		}
	}
	if cached[0].Action != updater.ActionUnchanged || patched[0].Action != updater.ActionUpdated || dns.findCalls != 1 {
		t.Fatalf("cached=%+v patched=%+v find=%d", cached[0], patched[0], dns.findCalls)
	}
}

// Two cfddns processes (the service's first cycle beside a manual -once, or
// cron beside a daemon) can both see no record and both create one. Every
// racer re-reads after creating and deletes its own record when a lower ID
// exists, so they converge on one record without either knowing the other.
func TestRun_CreateRaceDeletesOwnHigherDuplicate(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{zoneID: "zone-x"}
	dns.afterCreate = func() {
		other := cloudflare.Record{ID: "aaa-other", Type: "A", Name: domain, Content: "198.51.100.1"}
		dns.records["A|"+domain] = append([]cloudflare.Record{other}, dns.records["A|"+domain]...)
	}
	det := &fakeIP{v4: "198.51.100.1"}
	up := &updater.Updater{DomainV4: domain, IP: det, DNS: dns}

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionUnchanged || results[0].Warning != "" {
		t.Fatalf("results=%+v ok=%v", results, ok)
	}
	if len(dns.deletes) != 1 || dns.deletes[0] != "new1" {
		t.Fatalf("must delete exactly its own record: deletes=%v", dns.deletes)
	}
	if dns.findCalls != 2 {
		t.Fatalf("expected the lookup before create and the re-read after: find=%d", dns.findCalls)
	}
	// The survivor is what the cache manages from now on.
	det.v4 = "198.51.100.2"
	results, _ = up.Run(context.Background())
	if results[0].Action != updater.ActionUpdated || dns.findCalls != 2 {
		t.Fatalf("after reconcile: %+v find=%d", results, dns.findCalls)
	}
	if len(dns.updates) != 1 || dns.updates[0] != (updateCall{zoneID: "zone-x", id: "aaa-other", content: "198.51.100.2"}) {
		t.Fatalf("updates: %+v", dns.updates)
	}
}

func TestRun_CreateRaceKeepsOwnLowerRecord(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{}
	dns.afterCreate = func() {
		dns.records["A|"+domain] = append(dns.records["A|"+domain], cloudflare.Record{ID: "zzz-other", Type: "A", Name: domain, Content: "198.51.100.1"})
	}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionCreated {
		t.Fatalf("results=%+v ok=%v", results, ok)
	}
	if len(dns.deletes) != 0 {
		t.Fatalf("the lowest ID is never deleted: %v", dns.deletes)
	}
	// The sibling is still there at this instant; the result says so, like
	// any other duplicate, and the sibling's own reconcile removes it.
	if w := results[0].Warning; !strings.Contains(w, "2 A records") || !strings.Contains(w, "only new1") {
		t.Fatalf("warning = %q", w)
	}
}

func TestRun_CreateVerifyReadFailsStillCreated(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{findErrs: []error{nil, errors.New("cloudflare api: status 502")}}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionCreated {
		t.Fatalf("the record exists, the cycle must say created: %+v ok=%v", results, ok)
	}
	if !strings.Contains(results[0].Warning, "status 502") {
		t.Fatalf("the skipped duplicate check must be visible: %+v", results[0])
	}
	if len(dns.deletes) != 0 {
		t.Fatalf("nothing may be deleted blind: %v", dns.deletes)
	}
	// Cached as usual: the next cycle needs no lookup.
	if r, _ := up.Run(context.Background()); r[0].Action != updater.ActionUnchanged || dns.findCalls != 2 {
		t.Fatalf("after create: %+v find=%d", r, dns.findCalls)
	}
}

func TestRun_CreateVerifyReadEmptyStillCreated(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{}
	dns.afterCreate = func() { dns.records = nil } // the read lags the write
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}

	results, ok := up.Run(context.Background())
	if !ok || results[0].Action != updater.ActionCreated || results[0].Warning != "" {
		t.Fatalf("results=%+v ok=%v", results, ok)
	}
	if len(dns.deletes) != 0 {
		t.Fatalf("deletes: %v", dns.deletes)
	}
	if r, _ := up.Run(context.Background()); r[0].Action != updater.ActionUnchanged || dns.findCalls != 2 {
		t.Fatalf("after create: %+v find=%d", r, dns.findCalls)
	}
}

// Observe is the hand-off for anything stateful downstream (the log
// reporter). It runs inside the cycle lock, so observers see cycles in the
// order they ran and never two at once; the plain slice append below is
// only correct because of that, and -race proves it.
func TestRun_ObserveRunsSerializedWithTheCycle(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{records: oneRecord(domain, "rec-a", "A", "198.51.100.1")}
	var seen [][]updater.Result
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns,
		Observe: func(r []updater.Result) { seen = append(seen, r) }}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up.Run(context.Background())
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("observed %d cycles, want 20", len(seen))
	}
	for i, r := range seen {
		if len(r) != 1 || r[0].Family != "ipv4" {
			t.Fatalf("cycle %d: %+v", i, r)
		}
	}
}

func TestRun_ObserveSkipsLockWaitFailures(t *testing.T) {
	const domain = "home.example.com"
	block := make(chan struct{})
	observed := 0
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1", block: block}, DNS: &fakeDNS{},
		Observe: func([]updater.Result) { observed++ }}

	done := make(chan struct{})
	go func() {
		up.Run(context.Background())
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if results, ok := up.Run(ctx); ok || results[0].Action != updater.ActionError {
		t.Fatalf("waiter: %+v ok=%v", results, ok)
	}
	close(block)
	<-done
	// A waiter that gave up ran no cycle; only the blocked cycle is observed.
	if observed != 1 {
		t.Fatalf("observed %d, want 1", observed)
	}
}

func TestRun_CreateRaceDeleteFailureIsReported(t *testing.T) {
	const domain = "home.example.com"
	dns := &fakeDNS{deleteErr: errors.New("cloudflare api: status 503")}
	dns.afterCreate = func() {
		other := cloudflare.Record{ID: "aaa-other", Type: "A", Name: domain, Content: "198.51.100.1"}
		dns.records["A|"+domain] = append([]cloudflare.Record{other}, dns.records["A|"+domain]...)
	}
	up := &updater.Updater{DomainV4: domain, IP: &fakeIP{v4: "198.51.100.1"}, DNS: dns}

	results, ok := up.Run(context.Background())
	// Both records exist; the lowest is managed as for any duplicate, and
	// the result says which one is ours and why it is still there.
	if !ok || results[0].Action != updater.ActionUnchanged {
		t.Fatalf("results=%+v ok=%v", results, ok)
	}
	w := results[0].Warning
	for _, want := range []string{"new1", "aaa-other", "status 503", "only aaa-other is managed"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning %q lacks %q", w, want)
		}
	}
	if len(dns.deletes) != 1 || dns.deletes[0] != "new1" {
		t.Fatalf("deletes: %v", dns.deletes)
	}
}
func TestRun_Timestamps(t *testing.T) {
	const domain = "home.example.com"
	const addr = "198.51.100.1"
	zone := time.FixedZone("UTC+2", 2*60*60)
	injected := []time.Time{
		time.Date(2026, 10, 27, 12, 30, 1, 123456789, zone),
		time.Date(2026, 10, 27, 12, 30, 5, 987654321, zone),
	}
	nowCalls := 0
	up := &updater.Updater{
		DomainV4: domain,
		IP:       &fakeIP{v4: addr},
		DNS:      &fakeDNS{records: oneRecord(domain, "rec-a", "A", addr)},
		Now: func() time.Time {
			if nowCalls >= len(injected) {
				t.Fatalf("Now called %d times, want %d", nowCalls+1, len(injected))
			}
			now := injected[nowCalls]
			nowCalls++
			return now
		},
	}

	results, allOK := up.Run(context.Background())
	if !allOK || len(results) != 1 {
		t.Fatalf("results=%+v allOK=%v", results, allOK)
	}
	if nowCalls != 2 {
		t.Fatalf("Now calls=%d, want 2", nowCalls)
	}
	wantDetected := time.Date(2026, 10, 27, 10, 30, 1, 123456789, time.UTC)
	wantFinished := time.Date(2026, 10, 27, 10, 30, 5, 987654321, time.UTC)
	if got := results[0].DetectedAt; got == nil || !got.Equal(wantDetected) || got.Location() != time.UTC {
		t.Fatalf("DetectedAt=%v, want %v in UTC", got, wantDetected)
	}
	if got := results[0].FinishedAt; got == nil || !got.Equal(wantFinished) || got.Location() != time.UTC {
		t.Fatalf("FinishedAt=%v, want %v in UTC", got, wantFinished)
	}
}

func TestRun_DetectFailureHasTimestamps(t *testing.T) {
	up := &updater.Updater{
		DomainV4: "home.example.com",
		IP:       &fakeIP{v4: "203.0.113.10", v4Err: errors.New("detect failed")},
		DNS:      &fakeDNS{},
	}

	results, allOK := up.Run(context.Background())
	if allOK || len(results) != 1 {
		t.Fatalf("results=%+v allOK=%v", results, allOK)
	}
	r := results[0]
	if r.Action != updater.ActionError || r.Error == "" || r.IP != "" {
		t.Fatalf("detect failure result=%+v", r)
	}
	if r.DetectedAt == nil || r.FinishedAt == nil {
		t.Fatalf("detect failure timestamps: DetectedAt=%v FinishedAt=%v", r.DetectedAt, r.FinishedAt)
	}
}

func TestSelectManagedRecord_SortsAndWarns(t *testing.T) {
	const domain = "home.example.com"
	records := []cloudflare.Record{
		{ID: "b", Type: "A", Name: domain},
		{ID: "a", Type: "A", Name: domain},
	}

	rec, warning := updater.SelectManagedRecord(records, "A", domain)
	if rec.ID != "a" {
		t.Fatalf("selected ID=%q, want a", rec.ID)
	}
	const wantWarning = "2 A records named home.example.com; only a is managed, the other 1 keep their content"
	if warning != wantWarning {
		t.Fatalf("warning=%q, want %q", warning, wantWarning)
	}
}

func TestSelectManagedRecord_Empty(t *testing.T) {
	rec, warning := updater.SelectManagedRecord(nil, "A", "home.example.com")
	if rec != (cloudflare.Record{}) || warning != "" {
		t.Fatalf("record=%+v warning=%q, want zero record and empty warning", rec, warning)
	}
}

func TestResult_TimestampsNotInJSON(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	payload, err := json.Marshal(updater.Result{DetectedAt: &now, FinishedAt: &now})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, field := range []string{"DetectedAt", "FinishedAt"} {
		if strings.Contains(string(payload), field) {
			t.Errorf("JSON %s contains timestamp field %q", payload, field)
		}
	}
}
