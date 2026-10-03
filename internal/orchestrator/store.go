package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Work dir layout, relative to work/<case-id>/ (spec 002 §4.2).
const (
	// CaseFile is the resume cursor: the Case including its current phase.
	CaseFile = "case.json"
	// CaptureDir holds the llm/ and mcp/ capture streams. It is passed to the
	// llm capture middleware and to mcpclient, which own their subdirectories.
	CaptureDir = "captures"
	// ReportJSON and ReportText are the reporter's artifacts.
	ReportJSON = "report.json"
	ReportText = "report.txt"
)

// File modes for work dir entries. Captures and reports may contain incident
// detail, so files are owner-writable and world-readable, not world-writable.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// store owns one case's work dir. Every write goes through atomicWrite, so a
// crash leaves either the previous version of a file or the new one, never a
// torn one (spec 002 §4.2).
type store struct {
	// dir is work/<case-id>.
	dir string
	// rename swaps the temp file into place; os.Rename in production, and a
	// failing stub in the torn-write test.
	rename func(oldpath, newpath string) error
}

// newStore prepares the work dir skeleton for a case and returns its store.
func newStore(dir string, rename func(oldpath, newpath string) error) (*store, error) {
	if rename == nil {
		rename = os.Rename
	}
	for _, sub := range []string{
		checkpointDir,
		filepath.Join(CaptureDir, "llm"),
		filepath.Join(CaptureDir, "mcp"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, sub), dirPerm); err != nil {
			return nil, fmt.Errorf("creating work dir %s: %w", sub, err)
		}
	}
	return &store{dir: dir, rename: rename}, nil
}

// openStore returns a store for an existing case work dir, failing if it is
// not there — resuming a case that was never started is a user error, not
// something to paper over by creating an empty dir.
func openStore(dir string, rename func(oldpath, newpath string) error) (*store, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Resume reads case.json first, so reaching this means the dir went
		// away between the two calls. It is still an absent case, not a fault.
		return nil, fmt.Errorf("opening case work dir: %w: %w", ErrNoSuchCase, err)
	case err != nil:
		return nil, fmt.Errorf("opening case work dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("case work dir %s is not a directory", dir)
	}
	return newStore(dir, rename)
}

// path resolves a work-dir-relative name.
func (s *store) path(rel string) string { return filepath.Join(s.dir, rel) }

// writeJSON marshals v as indented JSON and writes it atomically to rel.
func (s *store) writeJSON(rel string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", rel, err)
	}
	return s.writeFile(rel, append(data, '\n'))
}

// writeFile writes data atomically to rel, creating parent directories.
func (s *store) writeFile(rel string, data []byte) error {
	path := s.path(rel)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", rel, err)
	}
	if err := atomicWrite(path, data, s.rename); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	return nil
}

// writeCase persists the case file — the resume cursor. It is written only
// after the phase checkpoint lands, so a crash between the two leaves a case
// pointing at a phase whose checkpoint already exists, which resume handles by
// skipping it (spec 002 §4.2).
func (s *store) writeCase(c Case) error { return s.writeJSON(CaseFile, c) }

// ReadCase loads the case file from a case work dir. It is the read-only door
// into a work dir: it creates nothing, so inspection commands cannot leave
// half-formed cases behind.
//
// The two ways it fails are reported apart, because they ask an operator for
// different fixes (issue #45): a case file that is not there is ErrNoSuchCase —
// the ID is wrong — while one that is there and does not decode is
// ErrUnusableCase. Any other read fault (a permission denial, a failing disk) is
// returned unclassified, so it keeps exit 1. That is not a claim that such a
// fault is transient — a case file owned by root is as permanent as damage. It
// is that this binary cannot tell which, and exit 1 is the arm for what it
// cannot classify; see the CLI's exitCode.
func ReadCase(caseDir string) (Case, error) {
	data, err := os.ReadFile(filepath.Join(caseDir, CaseFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Case{}, fmt.Errorf("reading %s: %w: %w", CaseFile, ErrNoSuchCase, err)
	case err != nil:
		return Case{}, fmt.Errorf("reading %s: %w", CaseFile, err)
	}
	var c Case
	if err := json.Unmarshal(data, &c); err != nil {
		return Case{}, fmt.Errorf("decoding %s: %w: %w", CaseFile, ErrUnusableCase, err)
	}
	return c, nil
}

// writeCheckpoint persists one phase checkpoint under its walk index.
func (s *store) writeCheckpoint(index int, cp Checkpoint) error {
	return s.writeJSON(filepath.Join(checkpointDir, checkpointName(index, cp.Phase)), cp)
}

// atomicWrite writes data to path via a temp file in the same directory:
// write → fsync → rename. The temp file is removed if anything fails before
// the rename, so a failed write leaves no debris and no partial file.
func atomicWrite(path string, data []byte, rename func(oldpath, newpath string) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := f.Chmod(filePerm); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("setting temp file mode: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry so the rename itself survives a crash.
// Failures are ignored on purpose: not every platform or filesystem permits
// opening and syncing a directory, and the rename is already atomic without
// it — this only tightens the durability window.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
