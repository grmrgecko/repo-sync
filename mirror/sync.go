package mirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// destLock is the exclusive hold a run has on its destination directory.
type destLock struct {
	file *os.File
	path string
}

// lockAttempts bounds the retries for a lock file removed between opening
// it and locking it. That window is a few instructions wide and only opens
// when another run finishes inside it, so a handful of attempts always
// resolves it.
const lockAttempts = 5

// lockDestination takes an exclusive lock under the destination directory
// so overlapping runs, such as from cron, cannot race on the same tree.
// The caller releases it with release.
func lockDestination(dest string) (*destLock, error) {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return nil, err
	}
	name := filepath.Join(dest, fetch.LockFileName)
	for attempt := 0; attempt < lockAttempts; attempt++ {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("destination %s is locked by another repo-sync run", dest)
		}

		// The lock only guards the tree while the file holding it is still
		// the one at the path. A run that finished between the open and the
		// lock above took its file with it, so this lock is on a file the
		// next run will never look at; open the replacement and lock that.
		held, herr := f.Stat()
		current, cerr := os.Stat(name)
		if herr == nil && cerr == nil && os.SameFile(held, current) {
			return &destLock{file: f, path: name}, nil
		}
		f.Close()
		log.WithField("path", name).Debug("Destination lock file was replaced while locking; retrying.")
	}
	return nil, fmt.Errorf("failed to lock destination %s", dest)
}

// release removes the lock file and then drops the lock, in that order, so
// the file is never removed out from under whichever run takes it next. It
// leaves nothing behind in the published tree, including when a run ends
// early on an interrupt.
func (l *destLock) release() {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.WithError(err).WithField("path", l.path).Warn("Failed to remove the destination lock file.")
	}
	l.file.Close()
}

// Summary reports what a whole run transferred, for callers that act on the
// result rather than read the log. A repository that failed part way through
// still counts what it transferred before failing, as those transfers changed
// the destination all the same.
type Summary struct {
	Repositories int
	Failed       int
	DryRun       bool
	Fetched      int64
	FetchedBytes int64
	Unchanged    int64
	Pruned       int64
	Planned      int64
	PlannedBytes int64
	// Missing counts the files repository metadata listed that the upstream
	// did not serve, whether or not the run tolerated their absence.
	Missing int64
}

// Changed reports whether the run altered the destination. A dry run reports
// what it would have done in the same counters, so it never counts as a
// change.
func (s *Summary) Changed() bool {
	if s.DryRun {
		return false
	}
	return s.Fetched > 0 || s.Pruned > 0
}

// add folds one repository's counters into the run totals.
func (s *Summary) add(c *fetch.Counters) {
	s.Fetched += c.Fetched.Load()
	s.FetchedBytes += c.FetchedBytes.Load()
	s.Unchanged += c.Unchanged.Load()
	s.Pruned += c.Pruned.Load()
	s.Planned += c.Planned.Load()
	s.PlannedBytes += c.PlannedBytes.Load()
	s.Missing += c.Missing.Load()
}

// Sync synchronizes every configured repository URL into the destination,
// continuing after per-repository failures so one bad repository does not
// block the rest. The returned summary covers every repository reached,
// including on the error paths, so a caller can still tell whether a failed
// run changed anything.
func Sync(ctx context.Context, opts *Options) (*Summary, error) {
	fetch.Reload()
	summary := &Summary{DryRun: opts.DryRun}

	// Hold the destination for the whole run, releasing it however the run
	// ends so no lock file is left in the published tree.
	lock, err := lockDestination(opts.Destination)
	if err != nil {
		return summary, err
	}
	defer lock.release()

	// Resolve the configured URLs into the repositories, and the loose
	// files, this run works on.
	repos, files, err := resolveTargets(ctx, opts)
	if err != nil {
		return summary, err
	}
	if len(repos) == 0 && len(files) == 0 {
		return summary, errors.New("no repositories to synchronize")
	}

	// Synchronize each repository in turn. Every repository's counters are
	// folded into the run totals before the next one resets them, whatever
	// its outcome.
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		log.WithFields(log.Fields{"url": repo.url, "type": repo.typ}).Info("Synchronizing repository.")
		fetch.Stats.Reset()
		start := time.Now()
		err := syncOne(ctx, repo.url, repo.typ, opts)
		summary.Repositories++
		summary.add(fetch.Stats)
		if err != nil {
			summary.Failed++
			if errors.Is(err, context.Canceled) {
				return summary, err
			}
			log.WithError(err).WithField("url", repo.url).Error("Repository synchronization failed.")
			continue
		}
		logSummary(repo.url, opts, time.Since(start))
	}

	// Mirror the loose files last: a repository sharing a directory with
	// them prunes against its own metadata, so files fetched before it ran
	// would be removed as stale.
	var fileErr error
	if len(files) > 0 && ctx.Err() == nil {
		log.WithField("files", len(files)).Info("Synchronizing files.")
		fetch.Stats.Reset()
		fileErr = syncFiles(ctx, files, opts)
		summary.add(fetch.Stats)
		if errors.Is(fileErr, context.Canceled) {
			return summary, fileErr
		}
		if fileErr != nil {
			log.WithError(fileErr).Error("File synchronization failed.")
		}
	}

	var errs []error
	if summary.Failed > 0 {
		errs = append(errs, fmt.Errorf("%d of %d repositories failed to synchronize", summary.Failed, len(repos)))
	}
	if fileErr != nil {
		errs = append(errs, fileErr)
	}
	return summary, errors.Join(errs...)
}

// resolveTargets expands the configured URLs into the repositories to
// synchronize and the loose files to mirror beside them. Discovery crawls
// for both; without it each URL is a repository, whose format is detected
// when the run did not name one.
func resolveTargets(ctx context.Context, opts *Options) ([]repoTarget, []fileTarget, error) {
	if opts.Discover {
		return discoverTargets(ctx, opts)
	}

	repos := make([]repoTarget, 0, len(opts.URLs))
	for _, repoURL := range opts.URLs {
		typ := opts.Type
		if typ == "" {
			detected, err := detectType(ctx, repoURL, opts.discoverTypes())
			if err != nil {
				return nil, nil, err
			}
			log.WithFields(log.Fields{"url": repoURL, "type": detected}).Info("Detected repository type.")
			typ = detected
		}
		repos = append(repos, repoTarget{url: repoURL, typ: typ})
	}
	return repos, nil, nil
}

// discoverTargets expands each configured URL into the repositories and
// loose files below it. Crawling a vendor archive costs hundreds of
// directory listings, so a recent crawl's results are reused instead, and
// the record of what was found is what tells this run which repositories
// the upstream has dropped since.
func discoverTargets(ctx context.Context, opts *Options) ([]repoTarget, []fileTarget, error) {
	cache := loadDiscoverCache(opts.Destination)
	settings := opts.discoverOptions()
	now := time.Now()

	var repos []repoTarget
	var files []fileTarget
	for _, base := range opts.URLs {
		found, cached := cache.reuse(base, settings, opts.DiscoverCache, now)
		if !cached {
			var err error
			if found, err = discoverRepos(ctx, base, opts); err != nil {
				return nil, nil, fmt.Errorf("discover repositories under %s: %w", base, err)
			}
			log.WithFields(log.Fields{
				"url":          base,
				"repositories": len(found.repos),
				"files":        len(found.files),
			}).Info("Discovered repositories.")

			// Withdrawing content is as destructive as pruning, so it is
			// gated on the same flag and waits out the same grace period.
			evictDiscovered(cache.update(base, settings, found, now, opts), opts)
		}
		repos = append(repos, found.repos...)
		files = append(files, found.files...)
	}
	cache.save(opts.DryRun)
	return repos, files, nil
}

// syncFiles mirrors the loose files a crawl matched. They sit outside any
// repository and carry no published checksum, so they revalidate against
// their modification time, and one withdrawn between the crawl and the
// fetch is not a failure. Nothing here prunes: the files are not described
// by any metadata that could say one no longer belongs.
func syncFiles(ctx context.Context, files []fileTarget, opts *Options) error {
	// Group by directory so each group shares one source and its request
	// paths stay relative to it, keeping the crawl's order.
	var dirs []string
	groups := map[string][]fileTarget{}
	for _, file := range files {
		if _, ok := groups[file.dirURL]; !ok {
			dirs = append(dirs, file.dirURL)
		}
		groups[file.dirURL] = append(groups[file.dirURL], file)
	}

	for _, dirURL := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		destDir, err := opts.repoDest(dirURL)
		if err != nil {
			return err
		}
		src := fetch.NewSource([]string{dirURL})
		var jobs []fetch.Job
		for _, file := range groups[dirURL] {
			dst, err := fetch.LocalJoin(destDir, file.name)
			if err != nil {
				return err
			}
			jobs = append(jobs, fetch.Job{ReqPath: url.PathEscape(file.name), Dst: dst, Optional: true})
		}
		if opts.DryRun {
			fetch.PlanJobs(jobs, opts.Verify, nil)
			continue
		}
		if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, nil, nil); err != nil {
			return fmt.Errorf("fetch files under %s: %w", dirURL, err)
		}
		log.WithFields(log.Fields{"url": dirURL, "files": len(jobs), "destination": destDir}).Debug("Mirrored files.")
	}
	return nil
}

// logSummary reports one repository's transfer activity after a successful
// synchronization.
func logSummary(repoURL string, opts *Options, elapsed time.Duration) {
	fields := log.Fields{
		"url":         repoURL,
		"fetched":     fetch.Stats.Fetched.Load(),
		"transferred": fetch.FormatBytes(fetch.Stats.FetchedBytes.Load()),
		"unchanged":   fetch.Stats.Unchanged.Load(),
		"duration":    elapsed.Round(time.Millisecond).String(),
	}
	// Report tolerated absences, so a repository that synchronized despite
	// them does not look completely clean in the log.
	if missing := fetch.Stats.Missing.Load(); missing > 0 {
		fields["missing"] = missing
	}
	if opts.DryRun {
		fields["would_fetch"] = fetch.Stats.Planned.Load()
		fields["would_transfer"] = fetch.FormatBytes(fetch.Stats.PlannedBytes.Load())
		if opts.Prune {
			fields["would_prune"] = fetch.Stats.Pruned.Load()
		}
		log.WithFields(fields).Info("Dry run complete.")
		return
	}
	if opts.Prune {
		fields["pruned"] = fetch.Stats.Pruned.Load()
	}
	log.WithFields(fields).Info("Repository synchronized.")
}

// syncOne resolves the source for a single repository URL and runs the
// format-specific synchronization. Destination paths derive from the layout
// URL, so a mirror list maps to the first mirror's path rather than the
// list's own URL.
func syncOne(ctx context.Context, repoURL string, typ RepoType, opts *Options) error {
	// Pin this repository's format on a copy, so a run covering several
	// formats reports each repository as what it is and never mutates the
	// caller's options.
	repoOpts := *opts
	repoOpts.Type = typ
	opts = &repoOpts

	src, layoutURL, err := resolveSource(ctx, repoURL, typ)
	if err != nil {
		return err
	}
	switch typ {
	case RepoRPM:
		destDir, err := opts.repoDest(layoutURL)
		if err != nil {
			return err
		}
		log.WithField("destination", destDir).Debug("Resolved repository destination.")
		return syncRPM(ctx, src, destDir, opts)
	case RepoDeb:
		return syncDeb(ctx, src, layoutURL, opts)
	case RepoArch:
		destDir, err := opts.repoDest(layoutURL)
		if err != nil {
			return err
		}
		log.WithField("destination", destDir).Debug("Resolved repository destination.")
		return syncArch(ctx, src, layoutURL, destDir, opts)
	case RepoApk:
		destDir, err := opts.repoDest(layoutURL)
		if err != nil {
			return err
		}
		log.WithField("destination", destDir).Debug("Resolved repository destination.")
		return syncApk(ctx, src, destDir, opts)
	default:
		return fmt.Errorf("unsupported repository type %q", typ)
	}
}

// SyncInto synchronizes one repository into an explicit destination
// directory, bypassing URL-derived destination mapping. It exists for the
// mirror server, whose destinations follow request paths rather than
// upstream URL paths. For deb the destination is the archive root
// directory; for other types it is the repository directory itself.
// Concurrent callers interleave into fetch.Stats, so the counters carry no
// per-repository meaning on this path and are neither reset nor reported.
// A trace enabled here still reports correct totals: it accumulates its own
// counters for the duration of the crawl rather than reading the shared
// collector.
func SyncInto(ctx context.Context, typ RepoType, src *fetch.Source, repoURL, dest string, opts *Options) error {
	// Copy before the deb path pins destBase so the caller's Options is
	// never mutated, keeping the struct safe to reuse or share.
	serverOpts := *opts
	opts = &serverOpts
	switch typ {
	case RepoRPM:
		return syncRPM(ctx, src, dest, opts)
	case RepoDeb:
		opts.destBase = dest
		return syncDeb(ctx, src, repoURL, opts)
	case RepoArch:
		return syncArch(ctx, src, repoURL, dest, opts)
	case RepoApk:
		return syncApk(ctx, src, dest, opts)
	default:
		return fmt.Errorf("unsupported repository type %q", typ)
	}
}
