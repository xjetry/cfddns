package updater

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"cfddns/internal/cloudflare"
	"cfddns/internal/ip"
)

// Result is the per-family outcome of one update cycle.
type Result struct {
	Family string `json:"family"`
	Domain string `json:"domain"`
	IP     string `json:"ip,omitempty"`
	// Source says where IP came from (see ip.Detection.Source).
	Source string `json:"source,omitempty"`
	Action string `json:"action"` // created|updated|unchanged|error
	Error  string `json:"error,omitempty"`
	// Warning reports a condition that did not stop the update, e.g. several
	// records sharing the name.
	Warning string `json:"warning,omitempty"`
}

const (
	ActionCreated   = "created"
	ActionUpdated   = "updated"
	ActionUnchanged = "unchanged"
	ActionError     = "error"
)

// RecheckEvery is how many consecutive cycles a family may be served from the
// cached zone/record IDs before the record is re-read from Cloudflare. The
// cache makes an unchanged IP cost zero API calls, at the price that a record
// edited in the dashboard is only corrected on the next recheck. 12 cycles is
// one hour at the default 5m period.
const RecheckEvery = 12

func (r Result) fail(err error) Result {
	r.Action = ActionError
	r.Error = err.Error()
	return r
}

// IPDetector obtains the public address for a given IP family.
type IPDetector interface {
	Detect(ctx context.Context, f ip.Family) (ip.Detection, error)
}

// DNS looks up zones and manages DNS records.
type DNS interface {
	ZoneIDFor(ctx context.Context, fqdn string) (string, error)
	FindRecords(ctx context.Context, zoneID, typ, name string) ([]cloudflare.Record, error)
	CreateRecord(ctx context.Context, zoneID, typ, name, content string) (cloudflare.Record, error)
	UpdateRecordContent(ctx context.Context, zoneID, recordID, content string) error
}

// familyCache is what a full resolve learned about one family's record.
// content is the value Cloudflare holds as far as this process knows (last
// read or last successfully pushed); served counts cycles answered from the
// cache since that resolve.
type familyCache struct {
	zoneID   string
	recordID string
	content  string
	served   int
}

// Updater orchestrates IP detection and DNS updates for configured families.
// Run calls are serialized (ticker vs HTTP) through a context-aware lock so a
// caller with a deadline gives up instead of queueing behind a stalled cycle.
type Updater struct {
	DomainV4 string
	DomainV6 string
	IP       IPDetector
	DNS      DNS

	initOnce sync.Once
	sem      chan struct{}
	cache    map[ip.Family]*familyCache
}

func (u *Updater) init() {
	u.initOnce.Do(func() {
		u.sem = make(chan struct{}, 1)
		u.cache = make(map[ip.Family]*familyCache)
	})
}

type familySpec struct {
	f       ip.Family
	domain  string
	recType string
}

func (u *Updater) families() []familySpec {
	var out []familySpec
	if u.DomainV4 != "" {
		out = append(out, familySpec{ip.IPv4, u.DomainV4, "A"})
	}
	if u.DomainV6 != "" {
		out = append(out, familySpec{ip.IPv6, u.DomainV6, "AAAA"})
	}
	return out
}

// Run performs one update cycle for every configured family (v4 then v6).
// Detection or DNS errors for one family do not abort the other; allOK is
// true only when every configured family has action != "error". If ctx ends
// while another cycle holds the lock, every family reports an error.
func (u *Updater) Run(ctx context.Context) (results []Result, allOK bool) {
	u.init()
	select {
	case u.sem <- struct{}{}:
		defer func() { <-u.sem }()
	case <-ctx.Done():
		for _, fs := range u.families() {
			r := Result{Family: fs.f.String(), Domain: fs.domain}
			results = append(results, r.fail(fmt.Errorf("waiting for a running update cycle: %w", ctx.Err())))
		}
		return results, false
	}

	allOK = true
	for _, fs := range u.families() {
		r := u.runFamily(ctx, fs)
		results = append(results, r)
		if r.Action == ActionError {
			allOK = false
		}
	}
	return results, allOK
}

func (u *Updater) runFamily(ctx context.Context, fs familySpec) Result {
	res := Result{Family: fs.f.String(), Domain: fs.domain}

	det, err := u.IP.Detect(ctx, fs.f)
	if err != nil {
		return res.fail(err)
	}
	res.IP, res.Source = det.Addr, det.Source

	if c := u.cache[fs.f]; c != nil && c.served < RecheckEvery {
		if c.content == det.Addr {
			c.served++
			res.Action = ActionUnchanged
			return res
		}
		if err := u.DNS.UpdateRecordContent(ctx, c.zoneID, c.recordID, det.Addr); err == nil {
			// A successful PATCH proves the cached IDs are still valid, so it
			// counts as a verification.
			c.content = det.Addr
			c.served = 0
			res.Action = ActionUpdated
			return res
		}
		// The cached IDs may be stale (record deleted or recreated); resolve
		// from scratch instead of reporting an error the resolve may fix.
	}
	return u.resolve(ctx, res, fs, det.Addr)
}

// resolve is the uncached path: zone lookup, record lookup, create or patch,
// then (re)fill the cache. Any failure leaves the family uncached so the next
// cycle starts from scratch.
func (u *Updater) resolve(ctx context.Context, res Result, fs familySpec, addr string) Result {
	delete(u.cache, fs.f)

	zoneID, err := u.DNS.ZoneIDFor(ctx, fs.domain)
	if err != nil {
		return res.fail(err)
	}
	records, err := u.DNS.FindRecords(ctx, zoneID, fs.recType, fs.domain)
	if err != nil {
		return res.fail(err)
	}

	if len(records) == 0 {
		rec, err := u.DNS.CreateRecord(ctx, zoneID, fs.recType, fs.domain, addr)
		if err != nil {
			return res.fail(err)
		}
		u.cache[fs.f] = &familyCache{zoneID: zoneID, recordID: rec.ID, content: addr}
		res.Action = ActionCreated
		return res
	}

	// API order is unspecified; sort by ID so the managed record is the same
	// one on every recheck instead of drifting between siblings.
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	rec := records[0]
	if len(records) > 1 {
		res.Warning = fmt.Sprintf("%d %s records named %s; only %s is managed, the other %d keep their content",
			len(records), fs.recType, fs.domain, rec.ID, len(records)-1)
	}

	if rec.Content == addr {
		res.Action = ActionUnchanged
	} else {
		if err := u.DNS.UpdateRecordContent(ctx, zoneID, rec.ID, addr); err != nil {
			return res.fail(err)
		}
		res.Action = ActionUpdated
	}
	u.cache[fs.f] = &familyCache{zoneID: zoneID, recordID: rec.ID, content: addr}
	return res
}
