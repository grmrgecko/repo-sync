package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/mirror"
)

// summaryFields parses a printed summary back into its fields.
func summaryFields(t *testing.T, out string) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("summary line %q is not a field", line)
		}
		fields[name] = value
	}
	return fields
}

// TestPrintSummary verifies the printed summary reports what a calling script
// keys off, and that a dry run never reports a change.
func TestPrintSummary(t *testing.T) {
	tests := []struct {
		name    string
		summary *mirror.Summary
		want    map[string]string
		absent  []string
	}{
		{
			name: "a run that transferred files reports a change",
			summary: &mirror.Summary{
				Repositories: 2,
				Fetched:      7,
				FetchedBytes: 4096,
				Unchanged:    31,
				Pruned:       2,
			},
			want: map[string]string{
				"Number of repositories":        "2",
				"Number of failed repositories": "0",
				"Number of files transferred":   "7",
				"Number of files pruned":        "2",
				"Total transferred file size":   "4096 bytes",
				"Number of files unchanged":     "31",
				"Repository changed":            "true",
			},
			absent: []string{"Dry run", "Number of files to transfer"},
		},
		{
			name:    "an idle run reports no change",
			summary: &mirror.Summary{Repositories: 1, Unchanged: 12},
			want: map[string]string{
				"Number of files transferred": "0",
				"Number of files pruned":      "0",
				"Repository changed":          "false",
			},
		},
		{
			name:    "pruning alone is a change",
			summary: &mirror.Summary{Repositories: 1, Pruned: 1},
			want: map[string]string{
				"Number of files pruned": "1",
				"Repository changed":     "true",
			},
		},
		{
			name:    "a failed run still reports its totals",
			summary: &mirror.Summary{Repositories: 3, Failed: 1, Fetched: 4},
			want: map[string]string{
				"Number of repositories":        "3",
				"Number of failed repositories": "1",
				"Repository changed":            "true",
			},
		},
		{
			name: "a dry run reports the work it planned but no change",
			summary: &mirror.Summary{
				Repositories: 1,
				DryRun:       true,
				Fetched:      4,
				Planned:      9,
				PlannedBytes: 8192,
			},
			want: map[string]string{
				"Dry run":                     "true",
				"Number of files to transfer": "9",
				"Total file size to transfer": "8192 bytes",
				"Repository changed":          "false",
			},
			// The metadata a dry run fetches must never be reported as
			// files it transferred.
			absent: []string{"Number of files transferred", "Number of files pruned"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printSummary(&out, tc.summary)
			got := summaryFields(t, out.String())
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s = %q, want %q", name, got[name], want)
				}
			}
			for _, name := range tc.absent {
				if _, ok := got[name]; ok {
					t.Errorf("%s was reported when it does not apply", name)
				}
			}
		})
	}
}

// TestPrintSummaryNone verifies a run with no summary to report prints
// nothing, rather than an empty set of fields a script would misread.
func TestPrintSummaryNone(t *testing.T) {
	var out bytes.Buffer
	printSummary(&out, nil)
	if out.Len() != 0 {
		t.Errorf("printed %q without a summary", out.String())
	}
}

// TestSyncArgsMissing verifies missing file handling falls back to the
// crawler configuration and that the flags override it.
func TestSyncArgsMissing(t *testing.T) {
	dest := t.TempDir()

	// An unset flag pair takes the configured defaults, which is what a run
	// without a config file gets from the built-in ones.
	args := SyncArgs{Args: []string{"https://example.com/repo", dest}, MissingRetries: -1}
	opts, err := args.options(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Missing != fetch.MissingRetry || opts.MissingRetries != 3 {
		t.Errorf("missing = %q after %d runs, want retry after 3", opts.Missing, opts.MissingRetries)
	}

	// Explicit flags win, including a zero retry budget, which accepts an
	// absence the first time it is seen.
	args = SyncArgs{Args: []string{"https://example.com/repo", dest}, Missing: "ignore", MissingRetries: 0}
	opts, err = args.options(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Missing != fetch.MissingIgnore || opts.MissingRetries != 0 {
		t.Errorf("missing = %q after %d runs, want ignore after 0", opts.Missing, opts.MissingRetries)
	}

	// An unsupported mode is rejected rather than silently ignored.
	args = SyncArgs{Args: []string{"https://example.com/repo", dest}, Missing: "skip", MissingRetries: -1}
	if _, err := args.options(nil); err == nil {
		t.Error("an unsupported missing mode was accepted")
	}
}
