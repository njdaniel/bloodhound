package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CheckpointSchemaVersion is the version stamped into every checkpoint file
// (spec 002 §4.2). It guards future migrations: a reader that does not
// recognize the version refuses the file instead of guessing.
const CheckpointSchemaVersion = 1

// Status is the outcome recorded in a checkpoint.
type Status string

// Checkpoint statuses. A completed checkpoint is loaded and never re-executed
// on resume; a failed one is overwritten when the phase runs again
// (spec 002 §4.3).
const (
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Checkpoint is the on-disk record of one phase attempt: checkpoints/NN-<phase>.json
// under the case work dir. It is the resume substrate and the cost ledger —
// `bloodhound cost` sums checkpoint spend rather than keeping a second record
// that could drift (spec 002 §4.2).
type Checkpoint struct {
	// SchemaVersion is CheckpointSchemaVersion.
	SchemaVersion int `json:"schema_version"`
	// CaseID is the case this checkpoint belongs to.
	CaseID string `json:"case_id"`
	// Phase is the phase that ran.
	Phase Phase `json:"phase"`
	// Status is StatusCompleted or StatusFailed.
	Status Status `json:"status"`
	// StartedAt and FinishedAt bracket the attempt, in UTC.
	//
	// They report what the clock said, so FinishedAt can precede StartedAt if
	// the clock stepped backwards mid-phase. Spend.WallMS does not follow it
	// negative — it is measured monotonically and clamped (issue #46) — so the
	// pair being inverted alongside a small or zero WallMS is the only trace
	// left that a step occurred, and is deliberately preserved as such.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Spend is what the phase consumed. Phases that make no model or tool
	// calls still record wall clock.
	Spend Spend `json:"spend"`
	// Output is the phase's result, deserialized on resume as that phase's
	// type: intake → Case, investigate → Finding, report → ReportOutput.
	Output json.RawMessage `json:"output"`
	// Error is the failure message on a failed checkpoint, and null otherwise.
	Error *string `json:"error"`
}

// ReportOutput is the report phase's checkpoint output: the artifacts it wrote
// under the case work dir.
type ReportOutput struct {
	// Artifacts are work-dir-relative paths, e.g. "report.json".
	Artifacts []string `json:"artifacts"`
}

// checkpointDir is the work-dir-relative directory holding checkpoint files.
const checkpointDir = "checkpoints"

// checkpointName renders the NN-<phase>.json filename for the index-th phase
// of the walk (1-based, matching spec 002 §4.2's 01-intake, 02-investigate…).
func checkpointName(index int, phase Phase) string {
	return fmt.Sprintf("%02d-%s.json", index, phase)
}

// LoadCheckpoints reads every checkpoint in caseDir/checkpoints, ordered by
// the leading sequence number in the filename. It is the reader behind both
// resume and `bloodhound cost`.
func LoadCheckpoints(caseDir string) ([]Checkpoint, error) {
	dir := filepath.Join(caseDir, checkpointDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading checkpoint dir: %w", err)
	}

	type numbered struct {
		seq int
		cp  Checkpoint
	}
	var found []numbered
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seq, err := checkpointSeq(e.Name())
		if err != nil {
			return nil, fmt.Errorf("checkpoint %s: %w", e.Name(), err)
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading checkpoint %s: %w", e.Name(), err)
		}
		var cp Checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			return nil, fmt.Errorf("decoding checkpoint %s: %w", e.Name(), err)
		}
		if cp.SchemaVersion != CheckpointSchemaVersion {
			return nil, fmt.Errorf("checkpoint %s: schema_version %d is not supported (want %d)",
				e.Name(), cp.SchemaVersion, CheckpointSchemaVersion)
		}
		// A checkpoint written before the issue #46 fix can carry a negative
		// WallMS. This is the single reader behind both resume and
		// `bloodhound cost`, so normalizing here is the one place that keeps
		// such a file from inflating a budget, a deadline, and the ledger.
		cp.Spend = cp.Spend.Normalize()
		found = append(found, numbered{seq: seq, cp: cp})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].seq < found[j].seq })

	out := make([]Checkpoint, 0, len(found))
	for _, f := range found {
		out = append(out, f.cp)
	}
	return out, nil
}

// checkpointSeq extracts the leading sequence number of a checkpoint filename.
func checkpointSeq(name string) (int, error) {
	dash := strings.IndexByte(name, '-')
	if dash <= 0 {
		return 0, fmt.Errorf("filename does not start with a sequence number")
	}
	seq, err := strconv.Atoi(name[:dash])
	if err != nil {
		return 0, fmt.Errorf("parsing sequence number: %w", err)
	}
	return seq, nil
}

// SumSpend totals the spend of the given checkpoints. Failed attempts count:
// a phase that burned tokens and then failed still cost money, and a resumed
// run must charge it against the budget (spec 002 §4.3).
func SumSpend(cps []Checkpoint) Spend {
	var total Spend
	for _, cp := range cps {
		total = total.Add(cp.Spend)
	}
	return total
}
