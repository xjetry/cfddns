// Package status maintains the optional runtime status snapshot.
package status

import (
	"errors"
	"os"
	"sync"
	"time"

	"cfddns/internal/updater"
)

// Config configures a Reporter.
type Config struct {
	Path     string
	Period   time.Duration
	DomainV4 string
	DomainV6 string
	Now      func() time.Time
}

// Reporter serializes in-memory updates and replacement of the status file.
type Reporter struct {
	mu       sync.Mutex
	path     string
	now      func() time.Time
	snapshot snapshot
}

type snapshot struct {
	Version   int            `json:"version"`
	StartedAt time.Time      `json:"started_at"`
	WrittenAt time.Time      `json:"written_at"`
	Period    string         `json:"period"`
	Families  []familyStatus `json:"families"`
}

type familyStatus struct {
	Family     string           `json:"family"`
	Domain     string           `json:"domain"`
	RecordType string           `json:"record_type"`
	Detection  *detectionStatus `json:"detection,omitempty"`
	Execution  *executionStatus `json:"execution,omitempty"`
}

type detectionStatus struct {
	At     time.Time `json:"at"`
	IP     string    `json:"ip"`
	Source string    `json:"source"`
	Error  string    `json:"error"`
}

type executionStatus struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Error   string    `json:"error"`
	Warning string    `json:"warning"`
}

// New captures the process start time and initializes the configured families.
func New(config Config) *Reporter {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	r := &Reporter{path: config.Path, now: now}
	r.snapshot = snapshot{
		Version:   1,
		StartedAt: now().UTC(),
		Period:    config.Period.String(),
		Families:  make([]familyStatus, 0, 2),
	}
	if config.DomainV4 != "" {
		r.snapshot.Families = append(r.snapshot.Families, familyStatus{
			Family: "ipv4", Domain: config.DomainV4, RecordType: "A",
		})
	}
	if config.DomainV6 != "" {
		r.snapshot.Families = append(r.snapshot.Families, familyStatus{
			Family: "ipv6", Domain: config.DomainV6, RecordType: "AAAA",
		})
	}
	return r
}

// Clear removes the status file. A missing file is already clear.
func (r *Reporter) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	err := os.Remove(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Report applies the supplied per-family results and atomically replaces the
// status file. Results for unconfigured families are ignored.
func (r *Reporter) Report(results []updater.Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, result := range results {
		for i := range r.snapshot.Families {
			family := &r.snapshot.Families[i]
			if family.Family != result.Family {
				continue
			}

			detectionFailed := result.DetectedAt != nil && result.Action == updater.ActionError && result.IP == ""
			if result.DetectedAt != nil {
				family.Detection = &detectionStatus{
					At:     result.DetectedAt.UTC(),
					IP:     result.IP,
					Source: result.Source,
				}
				if detectionFailed {
					family.Detection.Error = result.Error
				}
			}
			if result.FinishedAt != nil {
				family.Execution = &executionStatus{
					At:      result.FinishedAt.UTC(),
					Action:  result.Action,
					Warning: result.Warning,
				}
				if !detectionFailed {
					family.Execution.Error = result.Error
				}
			}
			break
		}
	}

	r.snapshot.WrittenAt = r.now().UTC()
	return writeJSONAtomic(r.path, r.snapshot)
}
