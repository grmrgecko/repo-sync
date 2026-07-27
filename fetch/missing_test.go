package fetch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readMissingState reads the records a run left in a repository.
func readMissingState(t *testing.T, root string) map[string]*missingRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, MissingStateName))
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]*missingRecord{}
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	return records
}

// TestParseMissingMode verifies mode names are accepted case insensitively
// and that an unconfigured mode is the strict one.
func TestParseMissingMode(t *testing.T) {
	cases := map[string]MissingMode{
		"":       MissingFail,
		"fail":   MissingFail,
		"retry":  MissingRetry,
		" Retry": MissingRetry,
		"ignore": MissingIgnore,
	}
	for name, want := range cases {
		got, err := ParseMissingMode(name)
		if err != nil || got != want {
			t.Errorf("ParseMissingMode(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	if _, err := ParseMissingMode("skip"); err == nil {
		t.Error("expected an unsupported mode name to be rejected")
	}
}

// TestMissingStrictMode verifies the strict mode tracks nothing and
// tolerates nothing, so every absence keeps failing the run.
func TestMissingStrictMode(t *testing.T) {
	root := t.TempDir()
	m := NewMissing(root, MissingFail, 3, false)
	if m != nil {
		t.Fatal("the strict mode installed a tracker")
	}
	if m.Tolerate("pool/gone.deb") {
		t.Error("the strict mode tolerated a missing file")
	}
	if err := m.Finish(); err != nil {
		t.Errorf("Finish = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(root, MissingStateName)); !os.IsNotExist(err) {
		t.Error("the strict mode wrote a state file")
	}
}

// TestMissingRetryBudget verifies an absence has to repeat across
// consecutive runs before it stops failing them, and that a file the
// upstream serves again starts its budget over.
func TestMissingRetryBudget(t *testing.T) {
	root := t.TempDir()
	const gone = "pool/main/h/hello/hello_1.0_amd64.deb"

	// The first two runs mirror the rest of the repository but still report
	// the absence as a failure.
	for run := 1; run <= 2; run++ {
		m := NewMissing(root, MissingRetry, 3, false)
		if !m.Tolerate(gone) {
			t.Fatalf("run %d aborted on a missing file", run)
		}
		if err := m.Finish(); err == nil {
			t.Errorf("run %d reported success while the file was still being retried", run)
		}
		if rec := readMissingState(t, root)[gone]; rec == nil || rec.Runs != run || rec.Known {
			t.Errorf("run %d recorded %+v", run, rec)
		}
	}

	// The third run exhausts the budget, so the file is accepted as gone.
	m := NewMissing(root, MissingRetry, 3, false)
	m.Tolerate(gone)
	if err := m.Finish(); err != nil {
		t.Errorf("run 3 still failed after the retry budget: %v", err)
	}
	if rec := readMissingState(t, root)[gone]; rec == nil || !rec.Known {
		t.Errorf("run 3 recorded %+v, want a known absence", rec)
	}

	// A later run that misses nothing clears the bookkeeping entirely.
	m = NewMissing(root, MissingRetry, 3, false)
	if err := m.Finish(); err != nil {
		t.Errorf("a run that missed nothing failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, MissingStateName)); !os.IsNotExist(err) {
		t.Error("records lingered after the upstream served everything again")
	}
}

// TestMissingRepeatedPath verifies a file listed by several indexes counts
// once per run, as the budget counts runs rather than references.
func TestMissingRepeatedPath(t *testing.T) {
	root := t.TempDir()
	m := NewMissing(root, MissingRetry, 3, false)
	m.Tolerate("pool/gone.deb")
	m.Tolerate("pool/gone.deb")
	if err := m.Finish(); err == nil {
		t.Fatal("a single run exhausted the retry budget")
	}
	if rec := readMissingState(t, root)["pool/gone.deb"]; rec == nil || rec.Runs != 1 {
		t.Errorf("recorded %+v, want one run", rec)
	}
}

// TestMissingIgnoreMode verifies absences never fail a run in the ignore
// mode, while still being recorded for whoever reads the repository.
func TestMissingIgnoreMode(t *testing.T) {
	root := t.TempDir()
	m := NewMissing(root, MissingIgnore, 3, false)
	if !m.Tolerate("pool/gone.deb") {
		t.Fatal("the ignore mode aborted on a missing file")
	}
	if err := m.Finish(); err != nil {
		t.Errorf("Finish = %v, want nil", err)
	}
	if rec := readMissingState(t, root)["pool/gone.deb"]; rec == nil {
		t.Error("the ignore mode recorded nothing")
	}
}

// TestMissingDryRun verifies a dry run reports absences without leaving
// bookkeeping behind, so it never spends a run's retry budget.
func TestMissingDryRun(t *testing.T) {
	root := t.TempDir()
	m := NewMissing(root, MissingRetry, 3, true)
	m.Tolerate("pool/gone.deb")
	if err := m.Finish(); err == nil {
		t.Error("a dry run hid a missing file")
	}
	if _, err := os.Stat(filepath.Join(root, MissingStateName)); !os.IsNotExist(err) {
		t.Error("a dry run wrote a state file")
	}
}
