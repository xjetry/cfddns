package updater

import (
	"context"
	"sync"

	"cfddns/internal/cloudflare"
	"cfddns/internal/ip"
)

// Result is the per-family outcome of one update cycle.
type Result struct {
	Family string `json:"family"`
	Domain string `json:"domain"`
	IP     string `json:"ip,omitempty"`
	Action string `json:"action"` // created|updated|unchanged|error
	Error  string `json:"error,omitempty"`
}

// IPDetector obtains the public address for a given IP family.
type IPDetector interface {
	Detect(ctx context.Context, f ip.Family) (string, error)
}

// DNS looks up zones and manages DNS records.
type DNS interface {
	ZoneIDFor(ctx context.Context, fqdn string) (string, error)
	FindRecord(ctx context.Context, zoneID, typ, name string) (*cloudflare.Record, error)
	CreateRecord(ctx context.Context, zoneID string, rec cloudflare.Record) error
	UpdateRecord(ctx context.Context, zoneID, id string, rec cloudflare.Record) error
}

// Updater orchestrates IP detection and DNS updates for configured families.
// A single mutex serializes concurrent Run calls (ticker vs HTTP).
type Updater struct {
	DomainV4 string
	DomainV6 string
	IP       IPDetector
	DNS      DNS
	mu       sync.Mutex
}

// Run performs one update cycle for every configured family (v4 then v6).
// Detection or DNS errors for one family do not abort the other; allOK is
// true only when every configured family has action != "error".
func (u *Updater) Run(ctx context.Context) (results []Result, allOK bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	allOK = true
	if u.DomainV4 != "" {
		r := u.runFamily(ctx, ip.IPv4, u.DomainV4, "A")
		results = append(results, r)
		if r.Action == "error" {
			allOK = false
		}
	}
	if u.DomainV6 != "" {
		r := u.runFamily(ctx, ip.IPv6, u.DomainV6, "AAAA")
		results = append(results, r)
		if r.Action == "error" {
			allOK = false
		}
	}
	return results, allOK
}

func (u *Updater) runFamily(ctx context.Context, f ip.Family, domain, recType string) Result {
	res := Result{
		Family: f.String(),
		Domain: domain,
	}

	addr, err := u.IP.Detect(ctx, f)
	if err != nil {
		res.Action = "error"
		res.Error = err.Error()
		return res
	}
	res.IP = addr

	zoneID, err := u.DNS.ZoneIDFor(ctx, domain)
	if err != nil {
		res.Action = "error"
		res.Error = err.Error()
		return res
	}

	rec, err := u.DNS.FindRecord(ctx, zoneID, recType, domain)
	if err != nil {
		res.Action = "error"
		res.Error = err.Error()
		return res
	}

	if rec != nil && rec.Content == addr {
		res.Action = "unchanged"
		return res
	}

	newRec := cloudflare.Record{
		Type:    recType,
		Name:    domain,
		Content: addr,
		Proxied: false,
		TTL:     1,
	}

	if rec == nil {
		if err := u.DNS.CreateRecord(ctx, zoneID, newRec); err != nil {
			res.Action = "error"
			res.Error = err.Error()
			return res
		}
		res.Action = "created"
		return res
	}

	if err := u.DNS.UpdateRecord(ctx, zoneID, rec.ID, newRec); err != nil {
		res.Action = "error"
		res.Error = err.Error()
		return res
	}
	res.Action = "updated"
	return res
}
