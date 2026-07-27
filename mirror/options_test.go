package mirror

import (
	"path/filepath"
	"testing"
)

// TestRepoDest verifies URL path copying, trimming, and flat placement.
func TestRepoDest(t *testing.T) {
	dest := t.TempDir()
	repoURL := "https://example.com/repos/CentOS/7/EA4/"
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"default", Options{Destination: dest}, filepath.Join(dest, "repos", "CentOS", "7", "EA4")},
		{"trim2", Options{Destination: dest, Trim: 2}, filepath.Join(dest, "7", "EA4")},
		{"trim-all", Options{Destination: dest, Trim: 10}, dest},
		{"flat", Options{Destination: dest, Flat: true}, dest},
	}
	for _, c := range cases {
		got, err := c.opts.repoDest(repoURL)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
