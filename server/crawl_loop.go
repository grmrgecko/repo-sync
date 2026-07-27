package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/mirror"
	"github.com/grmrgecko/repo-sync/state"
	log "github.com/sirupsen/logrus"
)

// lockMu guards the per-resource lock table and the in-flight crawl set.
var lockMu sync.Mutex

// resourceLock is one entry in the lock table; refs counts holders and
// waiters so the entry can be dropped once nobody references it.
type resourceLock struct {
	mu   sync.Mutex
	refs int
}

// locks serializes crawls of the same resource across request handlers and
// the scheduled loop.
var locks = map[string]*resourceLock{}

// lockResource acquires the lock for a resource key and returns its unlock
// function. Entries are reference counted and removed when the last holder
// or waiter releases, so evicted resources do not accumulate in the table.
func lockResource(key string) func() {
	lockMu.Lock()
	l := locks[key]
	if l == nil {
		l = &resourceLock{}
		locks[key] = l
	}
	l.refs++
	lockMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		lockMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(locks, key)
		}
		lockMu.Unlock()
	}
}

// inventoryEvictionGrace shields resources still advertised by a mount's
// configuration from eviction after requests stop.
const inventoryEvictionGrace = 72 * time.Hour

// inflight records resources with a crawl queued or running so concurrent
// dispatches from the request handlers and the scheduler collapse into one
// crawl. Guarded by lockMu.
var inflight = map[string]bool{}

// crawlWG tracks dispatched crawl goroutines so shutdown can wait for them
// before the final state flush records their outcomes.
var crawlWG sync.WaitGroup

// crawlCtx is the context dispatched crawls run under. CrawlLoop installs
// its own so shutdown cancels crawls; the background default covers tests
// and requests arriving before the loop starts.
// A pointer is stored rather than the interface itself: atomic.Value
// rejects a second store of a different concrete type, and the background
// default and the loop's cancelable context are different types.
var crawlCtx atomic.Pointer[context.Context]

func init() {
	ctx := context.Background()
	crawlCtx.Store(&ctx)
}

// Crawl slots bound how many crawls run at once; slotCond re-reads the
// configured limit on every wake so a reload takes effect.
var (
	slotMu     sync.Mutex
	slotCond   = sync.NewCond(&slotMu)
	slotsInUse int
)

// acquireCrawlSlot blocks until a crawl slot is free under the configured
// concurrency limit.
func acquireCrawlSlot() {
	slotMu.Lock()
	for slotsInUse >= cfg.C.Crawler.ConcurrentCrawls {
		slotCond.Wait()
	}
	slotsInUse++
	slotMu.Unlock()
}

// releaseCrawlSlot frees a crawl slot and wakes the waiters.
func releaseCrawlSlot() {
	slotMu.Lock()
	slotsInUse--
	slotMu.Unlock()
	slotCond.Broadcast()
}

// startCrawl dispatches an asynchronous crawl of one resource, collapsing
// duplicate dispatches and bounding concurrency. Failures are logged; the
// retry gating lives in the schedulers.
func startCrawl(res resource) {
	lockMu.Lock()
	if inflight[res.Key] {
		lockMu.Unlock()
		return
	}
	inflight[res.Key] = true
	lockMu.Unlock()

	crawlWG.Add(1)
	go func() {
		defer crawlWG.Done()
		defer func() {
			lockMu.Lock()
			delete(inflight, res.Key)
			lockMu.Unlock()
		}()
		acquireCrawlSlot()
		defer releaseCrawlSlot()
		ctx := *crawlCtx.Load()
		if ctx.Err() != nil {
			return
		}
		if err := crawlResource(ctx, res); err != nil {
			log.WithError(err).WithField("key", res.Key).Error("Repository crawl failed.")
		}
	}()
}

// WaitCrawls blocks until every dispatched crawl has finished. It is called
// at shutdown after the crawl context is canceled and before the final
// state flush.
func WaitCrawls() {
	crawlWG.Wait()
}

// crawlResource synchronizes one classified resource under its lock. Deb
// resources also hold an archive-root lock so suites sharing a pool never
// download the same file concurrently.
func crawlResource(ctx context.Context, res resource) error {
	if res.Kind == string(mirror.RepoDeb) && res.Root != res.Path {
		defer lockResource("deb-root:" + res.Root)()
	}
	defer lockResource(res.Key)()

	err := dispatchCrawl(ctx, res)
	state.S.MarkCrawled(res.Key, time.Now(), err)
	return err
}

// pathBelow reports whether a request path sits strictly below a base path.
func pathBelow(p, base string) bool {
	if p == base {
		return false
	}
	if base == "/" {
		return true
	}
	return strings.HasPrefix(p, base+"/")
}

// prunableTree reports whether a crawl may prune its destination tree. Every
// sync prunes the repository directory, which for deb is the suite directory
// and otherwise the repository's own path. A crawl's keep set covers only
// that one repository, so a tree that also holds another mount's content or
// another tracked repository must not be pruned: the neighbour's files are
// unreferenced there and would be deleted. This matters most for a
// repository sitting at the mirror root, whose tree is the whole cache.
func prunableTree(conf *cfg.Config, res resource) bool {
	for _, mount := range conf.Mounts {
		if pathBelow(mount.Path, res.Path) {
			log.WithFields(log.Fields{"key": res.Key, "mount": mount.Path}).
				Debug("Pruning disabled; the repository tree contains another mount.")
			return false
		}
	}
	for key, entry := range state.S.Snapshot() {
		if key == res.Key || entry.Kind == kindGeneric {
			continue
		}
		if pathBelow(entry.Path, res.Path) {
			log.WithFields(log.Fields{"key": res.Key, "nested": key}).
				Debug("Pruning disabled; the repository tree contains another repository.")
			return false
		}
	}
	return true
}

// traceInfo maps the configured trace section onto the mirror package's
// description of this mirror. The mapping is repeated rather than shared
// with the sync commands so the mirror package keeps its own trace type
// instead of taking the configuration as part of its API.
func traceInfo(t cfg.TraceConfig) mirror.TraceInfo {
	return mirror.TraceInfo{
		Host:       t.HostName(),
		Maintainer: t.Maintainer,
		Sponsor:    t.Sponsor,
		Country:    t.Country,
		Location:   t.Location,
		Throughput: t.Throughput,
	}
}

// dispatchCrawl runs the format-specific synchronization for a resource
// into the online tree.
func dispatchCrawl(ctx context.Context, res resource) error {
	online := cfg.C.OnlineDomain()
	mount, ok := cfg.C.MountFor(res.Path)
	if !ok {
		return fmt.Errorf("no mount configured for %s", res.Path)
	}

	// upstreamFor maps a request path onto the mount's upstream URL.
	upstreamFor := func(p string) string {
		rel := p
		if mount.Path != "/" {
			rel = strings.TrimPrefix(p, mount.Path)
		}
		return mount.Upstream + rel
	}

	missing, err := fetch.ParseMissingMode(cfg.C.Crawler.MissingMode)
	if err != nil {
		return err
	}

	// Generic files never reach the crawl path: they refresh on demand at
	// serve time and expire through the failure cache and state eviction.
	typ := mirror.RepoType(res.Kind)
	opts := &mirror.Options{
		Type:           typ,
		Workers:        cfg.C.Crawler.Workers,
		Prune:          prunableTree(cfg.C, res),
		PruneGrace:     cfg.C.Crawler.PruneGrace,
		Missing:        missing,
		MissingRetries: cfg.C.Crawler.MissingRetries,
		Trace:          cfg.C.Trace.Enabled,
		TraceInfo:      traceInfo(cfg.C.Trace),
	}

	// Deb destinations pin the archive root so pool files land beside
	// their suites; every other type syncs into its own directory.
	destPath := res.Path
	if typ == mirror.RepoDeb {
		destPath = res.Root
	}
	dest, err := fetch.LocalJoin(online.Root, destPath)
	if err != nil {
		return err
	}
	repoURL := upstreamFor(res.Path)
	return mirror.SyncInto(ctx, typ, fetch.NewSource([]string{repoURL}), repoURL, dest, opts)
}

// CrawlLoop keeps configured and discovered resources fresh until the
// context is canceled, starting with an immediate pass. Its context also
// governs request-dispatched crawls so shutdown cancels them.
func CrawlLoop(ctx context.Context) {
	crawlCtx.Store(&ctx)
	runScheduledCrawls(ctx)
	t := time.NewTicker(cfg.C.CrawlCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fetchFailures.Sweep(time.Now())
			runScheduledCrawls(ctx)
			// Re-arm from the active config so a reload takes effect.
			t.Reset(cfg.C.CrawlCheckInterval)
		}
	}
}

// runScheduledCrawls runs one pass of configured and request-discovered
// crawls.
func runScheduledCrawls(ctx context.Context) {
	crawlConfigured(ctx)
	crawlDue(ctx)
}

// configuredResource builds the resource for a mount's configured
// repository.
func configuredResource(repo cfg.MountRepoConfig) resource {
	root := repo.Path
	if repo.Type == string(mirror.RepoDeb) {
		if idx := strings.Index(repo.Path, "/dists/"); idx >= 0 {
			root = repo.Path[:idx]
			if root == "" {
				root = "/"
			}
		}
	}
	return resource{
		Kind:    repo.Type,
		Key:     repo.Type + ":" + repo.Path,
		Path:    repo.Path,
		Root:    root,
		ReqPath: repo.Path,
	}
}

// crawlConfigured keeps every mount-configured repository synchronized
// regardless of client traffic. Crawls are dispatched asynchronously so a
// slow repository cannot stall the scheduler loop.
func crawlConfigured(ctx context.Context) {
	now := time.Now()
	for _, mount := range cfg.C.Mounts {
		for _, repo := range mount.Repos {
			if ctx.Err() != nil {
				return
			}
			res := configuredResource(repo)
			state.S.MarkSeenInInventory(res.Kind, res.Key, res.Path, res.Root, now)
			entry, _ := state.S.Entry(res.Key)
			if !dueForInventory(entry, now) {
				continue
			}
			startCrawl(res)
		}
	}
}

// dueForInventory gates configured crawls on the standard interval with
// faster retries after failures.
func dueForInventory(entry state.Entry, now time.Time) bool {
	if entry.LastCrawled.IsZero() {
		return true
	}
	if !entry.NextCrawl.IsZero() {
		return !now.Before(entry.NextCrawl)
	}
	return now.Sub(entry.LastCrawled) >= cfg.C.RepoCrawlInterval
}

// refreshTier walks the cumulative schedule, returning the refresh
// interval matching how long ago the resource was requested, or stale once
// the whole budget has elapsed.
func refreshTier(sinceRequested time.Duration, schedule []time.Duration) (time.Duration, bool) {
	var cumulative time.Duration
	for _, step := range schedule {
		if step <= 0 {
			continue
		}
		cumulative += step
		if sinceRequested < cumulative {
			return step, false
		}
	}
	return 0, true
}

// refreshSchedule is the tier list: the active crawl interval followed by
// the configured aging backoff steps.
func refreshSchedule() []time.Duration {
	schedule := make([]time.Duration, 0, 1+len(cfg.C.Crawler.RefreshSchedule))
	schedule = append(schedule, cfg.C.RepoCrawlInterval)
	return append(schedule, cfg.C.Crawler.RefreshSchedule...)
}

// activeInInventory reports whether a mount still advertised the resource
// recently enough to shield it from eviction.
func activeInInventory(entry state.Entry, now time.Time) bool {
	if entry.LastSeenInInventory.IsZero() {
		return false
	}
	return now.Sub(entry.LastSeenInInventory) < inventoryEvictionGrace
}

// nextCrawlAt returns when a resource should next crawl: immediately when
// never crawled, at the failure retry gate after errors, else at the tier
// interval.
func nextCrawlAt(entry state.Entry, tierInterval time.Duration) time.Time {
	if entry.LastCrawled.IsZero() {
		return time.Time{}
	}
	if entry.LastError != "" && !entry.NextCrawl.IsZero() {
		return entry.NextCrawl
	}
	tierGate := entry.LastCrawled.Add(tierInterval)
	if !entry.NextCrawl.IsZero() && entry.NextCrawl.After(tierGate) {
		return entry.NextCrawl
	}
	return tierGate
}

// crawlDue refreshes request-discovered resources on the tier schedule and
// evicts those whose traffic stopped past the whole budget.
func crawlDue(ctx context.Context) {
	now := time.Now()
	schedule := refreshSchedule()
	for key, entry := range state.S.Snapshot() {
		if ctx.Err() != nil {
			return
		}
		// Inventory-only entries are managed by crawlConfigured.
		if entry.LastRequested.IsZero() {
			continue
		}
		interval, stale := refreshTier(now.Sub(entry.LastRequested), schedule)
		if stale {
			if activeInInventory(entry, now) {
				continue
			}
			evictStale(key, entry)
			continue
		}
		// Generic files refresh on demand at serve time.
		if entry.Kind == kindGeneric {
			continue
		}
		next := nextCrawlAt(entry, interval)
		if !next.IsZero() && now.Before(next) {
			continue
		}
		startCrawl(resource{Kind: entry.Kind, Key: key, Path: entry.Path, Root: entry.Root, ReqPath: entry.Path})
	}
}

// evictStale removes a resource whose requests stopped long enough ago,
// re-checking under the lock so a racing request wins.
func evictStale(key string, entry state.Entry) {
	// Deb resources share an archive root across sibling suites; hold that
	// lock too, matching crawlResource, so eviction never races an
	// in-flight sibling-suite crawl touching the shared pool.
	if entry.Kind == string(mirror.RepoDeb) && entry.Root != entry.Path {
		defer lockResource("deb-root:" + entry.Root)()
	}
	defer lockResource(key)()

	current, ok := state.S.Entry(key)
	if ok && current.LastRequested.After(entry.LastRequested) {
		return
	}
	target, err := fetch.LocalJoin(cfg.C.OnlineDomain().Root, entry.Path)
	if err == nil && entry.Path != "/" && entry.Path != "" {
		if err := os.RemoveAll(target); err != nil {
			log.WithError(err).WithField("path", target).Warn("Failed to evict stale resource.")
			return
		}
		log.WithFields(log.Fields{"key": key, "path": target}).Info("Evicted stale resource.")
	}
	state.S.Delete(key)
}
