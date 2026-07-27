package fetch

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Counters accumulates transfer activity for one repository's summary log
// line. Counters are atomic because download workers run concurrently.
type Counters struct {
	Fetched      atomic.Int64
	FetchedBytes atomic.Int64
	Unchanged    atomic.Int64
	Pruned       atomic.Int64
	Planned      atomic.Int64
	PlannedBytes atomic.Int64
	Missing      atomic.Int64
}

// Stats collects transfer activity for the repository currently
// synchronizing. The CLI synchronizes repositories sequentially, so one
// package-level collector suffices there. The server's concurrent crawls
// interleave into these counters, so their values are only meaningful in
// the sequential path; the server must not report them.
var Stats = &Counters{}

// statsKey carries a caller's own collector on a context.
type statsKey struct{}

// WithStats returns a context whose transfers accumulate into c as well as
// into Stats. It gives one synchronization its own totals even while other
// synchronizations run concurrently, which is what the mirror server's
// crawls and their trace files need.
func WithStats(ctx context.Context, c *Counters) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, statsKey{}, c)
}

// runStats returns the caller's collector carried by ctx, or nil when the
// caller installed none.
func runStats(ctx context.Context) *Counters {
	c, _ := ctx.Value(statsKey{}).(*Counters)
	return c
}

// recordFetched counts a completed download in the shared collector and in
// the caller's own, when one is installed.
func recordFetched(ctx context.Context, bytes int64) {
	Stats.Fetched.Add(1)
	Stats.FetchedBytes.Add(bytes)
	if c := runStats(ctx); c != nil {
		c.Fetched.Add(1)
		c.FetchedBytes.Add(bytes)
	}
}

// recordUnchanged counts a file that needed no transfer in both collectors.
func recordUnchanged(ctx context.Context) {
	Stats.Unchanged.Add(1)
	if c := runStats(ctx); c != nil {
		c.Unchanged.Add(1)
	}
}

// recordMissing counts a file the upstream did not serve in both
// collectors, whether or not the run tolerates its absence.
func recordMissing(ctx context.Context) {
	Stats.Missing.Add(1)
	if c := runStats(ctx); c != nil {
		c.Missing.Add(1)
	}
}

// Reset clears the collector for the next repository.
func (s *Counters) Reset() {
	s.Fetched.Store(0)
	s.FetchedBytes.Store(0)
	s.Unchanged.Store(0)
	s.Pruned.Store(0)
	s.Planned.Store(0)
	s.PlannedBytes.Store(0)
	s.Missing.Store(0)
}

// FormatBytes renders a byte count in a human readable unit.
func FormatBytes(n int64) string {
	const unit = 1024
	const prefixes = "KMGTPE"
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	// Cap the exponent at the last available prefix so an unexpectedly
	// large count can never index past the prefix table.
	for m := n / unit; m >= unit && exp < len(prefixes)-1; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), prefixes[exp])
}
