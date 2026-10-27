// Package inspector reads the current Cloudflare state without modifying DNS.
package inspector

import (
	"context"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/updater"
)

// DNS is the read-only subset of the Cloudflare client used by Inspector.
type DNS interface {
	ZoneIDFor(ctx context.Context, fqdn string) (string, error)
	FindRecords(ctx context.Context, zoneID, typ, name string) ([]cloudflare.Record, error)
}

// Result is the independently queried Cloudflare state for one IP family.
type Result struct {
	Family     string    `json:"family"`
	Domain     string    `json:"domain"`
	RecordType string    `json:"record_type"`
	Found      bool      `json:"found"`
	Content    string    `json:"content"`
	QueriedAt  time.Time `json:"queried_at"`
	Warning    string    `json:"warning"`
	Error      string    `json:"error"`
}

// Inspector queries configured families without using updater state or writes.
type Inspector struct {
	DomainV4 string
	DomainV6 string
	DNS      DNS
	// Now supplies result timestamps; nil uses time.Now.
	Now func() time.Time
}

type familySpec struct {
	family     string
	domain     string
	recordType string
}

func (i *Inspector) families() []familySpec {
	var families []familySpec
	if i.DomainV4 != "" {
		families = append(families, familySpec{family: "ipv4", domain: i.DomainV4, recordType: "A"})
	}
	if i.DomainV6 != "" {
		families = append(families, familySpec{family: "ipv6", domain: i.DomainV6, recordType: "AAAA"})
	}
	return families
}

func (i *Inspector) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

// Inspect queries every configured family in v4/v6 order. A family query
// failure is reported in its result and does not prevent the other query.
func (i *Inspector) Inspect(ctx context.Context) (results []Result, allOK bool) {
	allOK = true
	for _, family := range i.families() {
		result, queryOK := i.inspectFamily(ctx, family)
		results = append(results, result)
		if !queryOK {
			allOK = false
		}
	}
	return results, allOK
}

func (i *Inspector) inspectFamily(ctx context.Context, family familySpec) (result Result, queryOK bool) {
	result = Result{
		Family:     family.family,
		Domain:     family.domain,
		RecordType: family.recordType,
	}
	defer func() {
		result.QueriedAt = i.now()
	}()

	zoneID, err := i.DNS.ZoneIDFor(ctx, family.domain)
	if err != nil {
		result.Error = err.Error()
		return result, false
	}
	records, err := i.DNS.FindRecords(ctx, zoneID, family.recordType, family.domain)
	if err != nil {
		result.Error = err.Error()
		return result, false
	}

	record, warning := updater.SelectManagedRecord(records, family.recordType, family.domain)
	result.Warning = warning
	if len(records) != 0 {
		result.Found = true
		result.Content = record.Content
	}
	return result, true
}
