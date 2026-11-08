package updater

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

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
	// DetectedAt is when IP detection completed; it is nil if detection did not run.
	DetectedAt *time.Time `json:"-"`
	// FinishedAt is when this family's update cycle completed.
	FinishedAt *time.Time `json:"-"`
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
	DeleteRecord(ctx context.Context, zoneID, recordID string) error
}

// familyCache is what a full resolve learned about one family's record.
// content is the value Cloudflare holds as far as this process knows (last
// read or last successfully pushed); served counts cycles answered from the
// cache since that resolve. warning is the duplicate-record warning that
// resolve produced: a cached cycle repeats it, so every cycle's result (log
// line or HTTP body) states the condition, not only one in RecheckEvery.
type familyCache struct {
	zoneID   string
	recordID string
	content  string
	warning  string
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
	// Observe, when set, receives every cycle's results while the cycle
	// still holds the lock, so an observer sees cycles in the order they ran
	// and never two at once; a stateful observer (the log reporter) needs no
	// lock of its own for that reason. A Run that gave up waiting for the
	// lock ran no cycle and is not observed: its results go to its caller.
	Observe func(results []Result)
	// Now supplies result timestamps; nil uses time.Now.
	Now func() time.Time

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

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
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
			r = r.fail(fmt.Errorf("waiting for a running update cycle: %w", ctx.Err()))
			finishedAt := u.now()
			r.FinishedAt = &finishedAt
			results = append(results, r)
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
	if u.Observe != nil {
		u.Observe(results)
	}
	return results, allOK
}

func (u *Updater) runFamily(ctx context.Context, fs familySpec) (res Result) {
	res = Result{Family: fs.f.String(), Domain: fs.domain}
	defer func() {
		finishedAt := u.now()
		res.FinishedAt = &finishedAt
	}()

	det, err := u.IP.Detect(ctx, fs.f)
	detectedAt := u.now()
	res.DetectedAt = &detectedAt
	if err != nil {
		return res.fail(err)
	}
	res.IP, res.Source = det.Addr, det.Source

	if c := u.cache[fs.f]; c != nil && c.served < RecheckEvery {
		res.Warning = c.warning
		if c.content == det.Addr {
			c.served++
			res.Action = ActionUnchanged
			return res
		}
		err := u.DNS.UpdateRecordContent(ctx, c.zoneID, c.recordID, det.Addr)
		if err == nil {
			// A successful PATCH proves the cached IDs are still valid, so it
			// counts as a verification.
			c.content = det.Addr
			c.served = 0
			res.Action = ActionUpdated
			return res
		}
		if !cloudflare.IsNotFound(err) {
			// Rate limit, outage, auth: nothing says the record is gone, so
			// the IDs stay and the next cycle retries the one PATCH. A
			// resolve here would spend three more calls on an API that is
			// already refusing.
			return res.fail(err)
		}
		// The cached record no longer exists (deleted or recreated in the
		// dashboard); resolve from scratch, which may create it again.
	}
	return u.resolve(ctx, res, fs, det.Addr)
}

// SelectManagedRecord chooses the lowest-ID record and warns when sibling
// records with the same type and name will be left unmanaged.
func SelectManagedRecord(records []cloudflare.Record, typ, name string) (cloudflare.Record, string) {
	if len(records) == 0 {
		return cloudflare.Record{}, ""
	}

	// API order is unspecified; sort by ID so the managed record is the same
	// one on every recheck instead of drifting between siblings.
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	rec := records[0]
	if len(records) > 1 {
		return rec, fmt.Sprintf("%d %s records named %s; only %s is managed, the other %d keep their content",
			len(records), typ, name, rec.ID, len(records)-1)
	}
	return rec, ""
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

	createdID := ""
	if len(records) == 0 {
		rec, err := u.DNS.CreateRecord(ctx, zoneID, fs.recType, fs.domain, addr)
		if err != nil {
			return res.fail(err)
		}
		createdID = rec.ID
		records, res.Warning = u.afterCreate(ctx, zoneID, fs, rec)
	}

	rec, warning := SelectManagedRecord(records, fs.recType, fs.domain)
	if res.Warning == "" {
		res.Warning = warning
	}

	switch {
	case rec.ID == createdID:
		res.Action = ActionCreated
	case rec.Content == addr:
		res.Action = ActionUnchanged
	default:
		if err := u.DNS.UpdateRecordContent(ctx, zoneID, rec.ID, addr); err != nil {
			return res.fail(err)
		}
		res.Action = ActionUpdated
	}
	u.cache[fs.f] = &familyCache{zoneID: zoneID, recordID: rec.ID, content: addr, warning: res.Warning}
	return res
}

// afterCreate re-reads the name right after a POST and deletes the record
// this process just made when a lower ID exists. Cloudflare accepts several
// A/AAAA records of one name, and two cfddns processes (the service's first
// cycle beside a manual -once, cron beside a daemon) can both pass
// FindRecords before either POSTs; without this step both records would stay
// forever. Every racer applies the same rule, so all of them converge on the
// lowest ID without knowing of each other, and only the record this process
// created is ever deleted, because nothing else is known to be ours.
//
// The returned set is what the name holds now; the created record is always
// in it unless it was deleted here, even when the read lagged the write. A
// non-empty warning says what could not be checked or undone; the caller
// reports it in place of the generic duplicate warning.
func (u *Updater) afterCreate(ctx context.Context, zoneID string, fs familySpec, created cloudflare.Record) ([]cloudflare.Record, string) {
	records, err := u.DNS.FindRecords(ctx, zoneID, fs.recType, fs.domain)
	if err != nil {
		return []cloudflare.Record{created}, fmt.Sprintf("created %s but could not re-read %s records to rule out a duplicate from a concurrent run: %v",
			created.ID, fs.recType, err)
	}
	lowest := created.ID
	seen := false
	for _, r := range records {
		if r.ID == created.ID {
			seen = true
		}
		if r.ID < lowest {
			lowest = r.ID
		}
	}
	if !seen {
		records = append(records, created)
	}
	if lowest == created.ID {
		return records, ""
	}
	if err := u.DNS.DeleteRecord(ctx, zoneID, created.ID); err != nil {
		return records, fmt.Sprintf("created %s beside existing %s and could not delete the duplicate: %v; only %s is managed",
			created.ID, lowest, err, lowest)
	}
	kept := make([]cloudflare.Record, 0, len(records)-1)
	for _, r := range records {
		if r.ID != created.ID {
			kept = append(kept, r)
		}
	}
	return kept, ""
}
