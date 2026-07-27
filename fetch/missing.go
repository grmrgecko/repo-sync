package fetch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// MissingMode selects what a synchronization does with a file its
// repository metadata lists but the upstream does not serve. Published
// repositories are not always complete: an index can outlive the packages it
// references, and a whole repository is not worth abandoning over one of
// them.
type MissingMode string

const (
	// MissingFail stops a repository at the first missing file.
	MissingFail MissingMode = "fail"
	// MissingRetry keeps synchronizing the rest of the repository and fails
	// the run until the same file has been missing across enough consecutive
	// runs to be accepted as gone.
	MissingRetry MissingMode = "retry"
	// MissingIgnore skips missing files without ever failing the run.
	MissingIgnore MissingMode = "ignore"
)

// MissingModes lists every supported mode, in the order they escalate from
// tolerant to strict.
var MissingModes = []MissingMode{MissingIgnore, MissingRetry, MissingFail}

// MissingModeNames lists the supported mode names, for user facing messages.
func MissingModeNames() []string {
	names := make([]string, 0, len(MissingModes))
	for _, mode := range MissingModes {
		names = append(names, string(mode))
	}
	return names
}

// ParseMissingMode converts a configured mode name into a MissingMode. An
// empty name selects the strict mode, which is what a caller that configured
// none expects.
func ParseMissingMode(name string) (MissingMode, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return MissingFail, nil
	}
	mode := MissingMode(name)
	if !slices.Contains(MissingModes, mode) {
		return "", fmt.Errorf("unsupported missing file mode %q; expected one of %s",
			name, strings.Join(MissingModeNames(), ", "))
	}
	return mode, nil
}

// MissingStateName is the known-missing bookkeeping file kept in a
// synchronized repository.
const MissingStateName = ".repo-sync-missing.json"

// missingRecord is one file's absence: when it was first and last missed,
// how many consecutive runs have missed it, and whether it has been accepted
// as permanently gone.
type missingRecord struct {
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Runs  int       `json:"runs"`
	Known bool      `json:"known"`
}

// Missing tracks the files an upstream did not serve during one repository
// synchronization and decides whether those absences fail it. Records live
// in the repository itself so an absence that repeats across runs can be
// told apart from one that has only just appeared, which is the difference
// between a package that was withdrawn and an upstream having a bad day.
//
// A nil Missing is the strict mode: nothing is tracked and every absence
// fails the run.
type Missing struct {
	mode    MissingMode
	retries int
	root    string
	dryRun  bool

	// mu guards everything below; download workers report concurrently.
	mu      sync.Mutex
	records map[string]*missingRecord
	seen    map[string]bool
	// unresolved counts the files missed this run that are still inside
	// their retry budget, and first names one of them for the error.
	unresolved int
	first      string
}

// NewMissing starts tracking the files an upstream does not serve for a
// repository rooted at dir, loading what earlier runs recorded there. The
// strict mode tracks nothing, so it returns nil and every absence keeps
// failing the run.
func NewMissing(dir string, mode MissingMode, retries int, dryRun bool) *Missing {
	if mode != MissingRetry && mode != MissingIgnore {
		return nil
	}
	m := &Missing{
		mode:    mode,
		retries: retries,
		root:    dir,
		dryRun:  dryRun,
		records: map[string]*missingRecord{},
		seen:    map[string]bool{},
	}
	m.load()
	return m
}

// load reads the records an earlier run left in the repository. A missing or
// unreadable file starts fresh: the records only decide how much longer an
// absence is retried, so losing them costs retries rather than correctness.
func (m *Missing) load() {
	data, err := os.ReadFile(filepath.Join(m.root, MissingStateName))
	if err != nil {
		return
	}
	records := map[string]*missingRecord{}
	if err := json.Unmarshal(data, &records); err != nil {
		log.WithError(err).WithField("path", m.root).Warn("Failed to parse the missing file state; starting fresh.")
		return
	}
	// Drop null records a hand-edited file may carry, as every lookup
	// dereferences the stored pointer.
	for name, rec := range records {
		if rec != nil {
			m.records[name] = rec
		}
	}
}

// Tolerate records that the upstream did not serve reqPath and reports
// whether the synchronization may continue without it. A file inside its
// retry budget is still tolerated for this run, so the rest of the
// repository is mirrored either way; Finish is what reports it as a failure.
func (m *Missing) Tolerate(reqPath string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Count each file once per run: a repository may list the same file from
	// several indexes, and the budget counts runs rather than references.
	if m.seen[reqPath] {
		return true
	}
	m.seen[reqPath] = true

	now := time.Now()
	rec := m.records[reqPath]
	if rec == nil {
		rec = &missingRecord{First: now}
		m.records[reqPath] = rec
	}
	rec.Runs++
	rec.Last = now

	// A file missing for its whole retry budget is accepted as gone and
	// stops failing runs from here on. It is still requested every run, so
	// an upstream that publishes it again is picked up without intervention.
	if rec.Runs >= m.retries {
		rec.Known = true
	}
	if m.mode == MissingIgnore || rec.Known {
		log.WithField("path", reqPath).Debug("Skipped a file the upstream does not serve.")
		return true
	}

	m.unresolved++
	if m.first == "" {
		m.first = reqPath
	}
	log.WithFields(log.Fields{"path": reqPath, "runs": rec.Runs, "retries": m.retries}).
		Warn("Upstream does not serve a file its metadata lists.")
	return true
}

// Finish persists what this run observed and reports the absences that have
// not yet been accepted as permanent, which is what fails the repository
// while a missing file is still being retried.
func (m *Missing) Finish() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.save()
	if m.unresolved == 0 {
		return nil
	}
	return fmt.Errorf("%d file(s) listed by the metadata are missing upstream and will be retried, starting with %s",
		m.unresolved, m.first)
}

// save rewrites the records for the files missed this run, and removes the
// file once nothing is missing. Only files missed this run are kept:
// anything else is served again, or is no longer listed at all, and either
// way its retry budget starts over.
func (m *Missing) save() {
	if m.dryRun {
		return
	}
	name := filepath.Join(m.root, MissingStateName)
	if len(m.seen) == 0 {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.WithError(err).WithField("path", name).Warn("Failed to remove the missing file state.")
		}
		return
	}
	kept := make(map[string]*missingRecord, len(m.seen))
	for reqPath := range m.seen {
		kept[reqPath] = m.records[reqPath]
	}
	data, err := json.Marshal(kept)
	if err == nil {
		err = os.MkdirAll(m.root, 0755)
	}
	if err == nil {
		err = os.WriteFile(name, data, 0644)
	}
	if err != nil {
		log.WithError(err).WithField("path", name).Warn("Failed to save the missing file state.")
	}
}
