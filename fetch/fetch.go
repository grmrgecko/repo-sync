// Package fetch downloads repository files over HTTP with mirror failover,
// verifying published sizes and checksums, staging updates so a live tree
// is never served a partially updated repository, and pruning files the
// upstream no longer lists.
package fetch

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
	"github.com/ulikunitz/xz"
)

// client is the shared HTTP client used for all repository fetches. It is
// atomic because a SIGHUP reload swaps it while requests are in flight.
var client atomic.Pointer[http.Client]

func init() {
	// Build a client immediately so the package is usable without an
	// explicit Reload. The config package is initialized first, so its
	// defaults are already installed.
	Reload()
}

// Reload rebuilds the shared HTTP client from the active crawler
// configuration. A header timeout is used instead of a whole-request
// timeout so large package downloads are not cut short.
func Reload() {
	old := client.Load()
	client.Store(&http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: cfg.C.Crawler.RequestTimeout,
		},
	})
	// Release the replaced transport's idle pool; in-flight requests on it
	// finish undisturbed.
	if old != nil {
		old.CloseIdleConnections()
	}
}

// ErrNotFound marks an upstream file that does not exist, letting callers
// treat missing optional files differently from transport failures.
var ErrNotFound = errors.New("upstream file not found")

// ErrNotModified marks a conditional request the upstream answered with
// 304, meaning the local copy is already current.
var ErrNotModified = errors.New("upstream file not modified")

// StagedSuffix is appended to staged downloads until they are promoted.
const StagedSuffix = ".staged"

// PartialSuffix is appended to in-progress downloads. Partials of files
// with published checksums survive failures so the next run can resume.
const PartialSuffix = ".partial"

// LockFileName is the flock target guarding a destination directory.
const LockFileName = ".repo-sync.lock"

// PruneStateName is the prune grace bookkeeping file kept at the root of a
// pruned tree.
const PruneStateName = ".repo-sync-prune.json"

// DiscoverStateName is the discovery cache kept at the root of a
// destination tree, recording what each crawl found and when.
const DiscoverStateName = ".repo-sync-state.json"

// Expect describes the size and checksums a fetched file must match. A size
// of -1 means the size is unknown, and Sums maps lowercase algorithm names
// to hex digests.
type Expect struct {
	Size int64
	Sums map[string]string
}

// MakeExpect builds an Expect from a size and a single checksum, treating
// non-positive sizes and empty checksums as unknown.
func MakeExpect(size int64, algo, digest string) *Expect {
	e := &Expect{Size: size, Sums: map[string]string{}}
	if size <= 0 {
		e.Size = -1
	}
	if algo != "" && digest != "" {
		e.Sums[strings.ToLower(algo)] = strings.ToLower(digest)
	}
	return e
}

// hashOrder lists checksum algorithms from most to least preferred.
var hashOrder = []string{"sha512", "sha384", "sha256", "sha224", "sha1", "sha", "md5", "md5sum"}

// newHasher returns a hash implementation for a checksum algorithm name as
// found in repository metadata, or nil when the algorithm is unknown.
func newHasher(algo string) hash.Hash {
	switch strings.ToLower(algo) {
	case "md5", "md5sum":
		return md5.New()
	case "sha", "sha1":
		return sha1.New()
	case "sha224":
		return sha256.New224()
	case "sha256":
		return sha256.New()
	case "sha384":
		return sha512.New384()
	case "sha512":
		return sha512.New()
	}
	return nil
}

// bestSum picks the strongest checksum available in the expectation.
func (e *Expect) bestSum() (string, string) {
	for _, algo := range hashOrder {
		if digest, ok := e.Sums[algo]; ok && digest != "" {
			return algo, digest
		}
	}
	return "", ""
}

// FileState reports what File did with a file. It is only meaningful when
// the accompanying error is nil.
type FileState int

const (
	// FileMissing marks an optional file the upstream does not serve.
	FileMissing FileState = iota
	// FileUnchanged marks an existing local file that matched expectations.
	FileUnchanged
	// FileFetched marks a file downloaded into its final location.
	FileFetched
	// FileStaged marks a file downloaded next to its final location for a
	// later promote.
	FileStaged
)

// verifyLocal reports whether an existing regular file already matches the
// expected size and, when checkHash is set, the expected checksum.
func verifyLocal(name string, want *Expect, checkHash bool) bool {
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if want.Size >= 0 && info.Size() != want.Size {
		return false
	}
	if !checkHash {
		return true
	}
	algo, digest := want.bestSum()
	if digest == "" {
		return true
	}
	sum, err := hashFile(name, algo)
	if err != nil {
		return false
	}
	return sum == digest
}

// hashFile computes the named checksum of a file, returned as lowercase hex.
func hashFile(name, algo string) (string, error) {
	h := newHasher(algo)
	if h == nil {
		return "", fmt.Errorf("unsupported checksum algorithm %q", algo)
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileStripes bounds the destination lock table. Downloads to the same
// destination are serialized by stripe; a collision between unrelated
// destinations only delays a download briefly.
const fileStripes = 512

// fileLocks serializes downloads by destination path. The mirror server's
// on-demand fetches and background crawls can target the same file
// concurrently, and their partial downloads must not interleave.
var fileLocks [fileStripes]sync.Mutex

// lockFile holds the lock for a destination path and returns its unlock
// function.
func lockFile(dst string) func() {
	h := fnv.New32a()
	_, _ = io.WriteString(h, dst)
	m := &fileLocks[h.Sum32()%fileStripes]
	m.Lock()
	return m.Unlock
}

// File downloads reqPath from the source into dst, verifying size and
// checksum when known. Existing files that already match are left alone.
// With stage set the download is written next to dst for a later promote so
// readers of a live tree never see a partially updated repository.
func File(ctx context.Context, src *Source, reqPath, dst string, want *Expect, stage, verifyExisting bool) (FileState, error) {
	// Serialize writers to this destination so concurrent callers, such
	// as the mirror server's on-demand fetches and background crawls,
	// cannot interleave partial downloads of the same file.
	unlock := lockFile(dst)
	defer unlock()

	// Clear any staged leftover from an interrupted run so a later promote
	// can never publish stale content.
	if stage {
		_ = os.Remove(dst + StagedSuffix)
	}

	// Skip files that already match the expected content.
	if want != nil && verifyLocal(dst, want, verifyExisting) {
		recordUnchanged(ctx)
		return FileUnchanged, nil
	}

	// Checksummed files with a known size can resume a failed download's
	// partial file with a ranged request.
	var resumeFrom int64
	if want != nil && want.Size > 0 {
		if _, digest := want.bestSum(); digest != "" {
			if info, err := os.Stat(dst + PartialSuffix); err == nil && info.Size() > 0 && info.Size() < want.Size {
				resumeFrom = info.Size()
			}
		}
	}

	for {
		state, err := download(ctx, src, reqPath, dst, want, stage, resumeFrom)
		if err != nil && resumeFrom > 0 && !errors.Is(err, ErrNotFound) && ctx.Err() == nil {
			// The partial may not be a prefix of the current upstream file;
			// retry once from scratch.
			log.WithError(err).WithField("path", reqPath).Debug("Resumed download failed; retrying in full.")
			_ = os.Remove(dst + PartialSuffix)
			resumeFrom = 0
			continue
		}
		return state, err
	}
}

// download performs one transfer attempt for File, appending to the
// partial file when resuming from a prior failure.
func download(ctx context.Context, src *Source, reqPath, dst string, want *Expect, stage bool, resumeFrom int64) (FileState, error) {
	// Files without published checksums cannot be verified locally, so an
	// existing copy is revalidated with a conditional request instead of
	// being re-downloaded every run.
	var modifiedSince time.Time
	if want == nil {
		if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() {
			modifiedSince = info.ModTime()
		}
	}

	// Request the file, failing over between mirrors as needed.
	resp, err := src.Get(ctx, reqPath, GetOptions{ModifiedSince: modifiedSince, RangeFrom: resumeFrom})
	if errors.Is(err, ErrNotModified) {
		recordUnchanged(ctx)
		log.WithField("path", reqPath).Debug("Kept unchanged repository file.")
		return FileUnchanged, nil
	}
	if err != nil {
		return FileMissing, err
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return FileMissing, err
	}

	var hasher hash.Hash
	var algo, digest string
	if want != nil {
		algo, digest = want.bestSum()
		if digest != "" {
			hasher = newHasher(algo)
		}
	}
	// Partials are only kept for files whose content the next attempt can
	// verify by checksum.
	keepPartial := digest != "" && want.Size > 0

	// A 206 continues the partial file, whose existing bytes must feed the
	// hash first; any other success replaces it from the start.
	partial := dst + PartialSuffix
	resumed := resumeFrom > 0 && resp.StatusCode == http.StatusPartialContent
	openFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if resumed {
		openFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if hasher != nil {
			pf, err := os.Open(partial)
			if err != nil {
				return FileMissing, err
			}
			_, err = io.Copy(hasher, pf)
			pf.Close()
			if err != nil {
				return FileMissing, err
			}
		}
		log.WithFields(log.Fields{"path": reqPath, "offset": resumeFrom}).Debug("Resuming partial download.")
	}

	// Stream the body into the partial file so a crash never leaves an
	// incomplete file at the final location.
	f, err := os.OpenFile(partial, openFlags, 0644)
	if err != nil {
		return FileMissing, err
	}
	w := io.Writer(f)
	if hasher != nil {
		w = io.MultiWriter(f, hasher)
	}
	written, copyErr := io.Copy(w, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		if !keepPartial {
			_ = os.Remove(partial)
		}
		return FileMissing, fmt.Errorf("download %s: %w", reqPath, copyErr)
	}
	if closeErr != nil {
		if !keepPartial {
			_ = os.Remove(partial)
		}
		return FileMissing, closeErr
	}

	// Verify the transfer against the published size and checksum. A
	// mismatch invalidates the partial file entirely.
	total := written
	if resumed {
		total += resumeFrom
	}
	if want != nil && want.Size >= 0 && total != want.Size {
		_ = os.Remove(partial)
		return FileMissing, fmt.Errorf("download %s: size %d does not match expected %d", reqPath, total, want.Size)
	}
	if hasher != nil {
		if sum := hex.EncodeToString(hasher.Sum(nil)); sum != digest {
			_ = os.Remove(partial)
			return FileMissing, fmt.Errorf("download %s: %s checksum %s does not match expected %s", reqPath, algo, sum, digest)
		}
	}

	// Move the download into place, or next to it when staging.
	final := dst
	state := FileFetched
	if stage {
		final = dst + StagedSuffix
		state = FileStaged
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return FileMissing, err
	}

	// Carry the upstream modification time so later runs can revalidate
	// with conditional requests. Promotion preserves it.
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		_ = os.Chtimes(final, time.Now(), lm)
	}
	recordFetched(ctx, written)
	log.WithField("path", reqPath).Debug("Fetched repository file.")
	return state, nil
}

// PlanJobs records what a real run would download without transferring
// anything, still marking every destination as part of the repository so a
// dry-run prune report stays accurate. Optional files the upstream may not
// serve are counted, so the result is an upper bound.
func PlanJobs(jobs []Job, verifyExisting bool, keep *KeepSet) {
	for _, job := range jobs {
		keep.Add(job.Dst)
		if job.Want != nil && verifyLocal(job.Dst, job.Want, verifyExisting) {
			Stats.Unchanged.Add(1)
			continue
		}
		Stats.Planned.Add(1)
		if job.Want != nil && job.Want.Size > 0 {
			Stats.PlannedBytes.Add(job.Want.Size)
		}
		log.WithField("path", job.ReqPath).Debug("Would fetch repository file.")
	}
}

// RemoveStale deletes a local file the upstream no longer serves, so an
// outdated copy is never served beside refreshed metadata. Missing files
// are not an error.
func RemoveStale(name string) {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.WithError(err).WithField("path", name).Warn("Failed to remove stale file.")
	}
}

// StagedOrFinal returns the staged copy of dst when one exists, falling
// back to the promoted file for runs where the upstream was unchanged.
func StagedOrFinal(dst string) string {
	staged := dst + StagedSuffix
	if _, err := os.Stat(staged); err == nil {
		return staged
	}
	return dst
}

// PromoteStaged moves a staged download into its final location. Missing
// staged files are not an error, as unchanged files are never staged.
func PromoteStaged(dst string) error {
	staged := dst + StagedSuffix
	if _, err := os.Stat(staged); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.Rename(staged, dst)
}

// PromoteOrDiscard finishes a staged file: promoted normally, or removed in
// a dry run so no published repository state changes.
func PromoteOrDiscard(dst string, dryRun bool) error {
	if dryRun {
		_ = os.Remove(dst + StagedSuffix)
		return nil
	}
	return PromoteStaged(dst)
}

// Job describes one file for Many to download.
type Job struct {
	ReqPath  string
	Dst      string
	Want     *Expect
	Optional bool
	Stage    bool
}

// Many downloads jobs with a fixed worker pool, recording each job's
// resulting state. The first hard failure aborts the remaining work. Files
// the upstream does not serve abort it too, unless the job is optional or
// miss tolerates the absence.
func Many(ctx context.Context, src *Source, jobs []Job, workers int, verifyExisting bool, keep *KeepSet, miss *Missing) ([]FileState, error) {
	states := make([]FileState, len(jobs))
	indexes := make(chan int)
	errs := make(chan error, 1)
	var wg sync.WaitGroup

	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range indexes {
				job := jobs[idx]
				state, err := File(ctx, src, job.ReqPath, job.Dst, job.Want, job.Stage, verifyExisting)
				if errors.Is(err, ErrNotFound) {
					if job.Optional {
						log.WithField("path", job.ReqPath).Debug("Skipped file the upstream does not serve.")
						continue
					}
					// A file the metadata lists is expected to exist, so its
					// absence is counted and reported however it is handled.
					recordMissing(ctx)
					if miss.Tolerate(job.ReqPath) {
						continue
					}
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				states[idx] = state
				keep.Add(job.Dst)
			}
		}()
	}

	for idx := range jobs {
		select {
		case err := <-errs:
			close(indexes)
			wg.Wait()
			return states, err
		case <-ctx.Done():
			close(indexes)
			wg.Wait()
			return states, ctx.Err()
		case indexes <- idx:
		}
	}
	close(indexes)
	wg.Wait()

	select {
	case err := <-errs:
		return states, err
	default:
		return states, nil
	}
}

// KeepSet tracks the files that belong to the current sync so pruning can
// remove everything else. A nil KeepSet ignores all operations.
type KeepSet struct {
	mu    sync.Mutex
	paths map[string]bool
}

// NewKeepSet returns a tracking set when pruning is enabled, or nil so all
// tracking becomes a no-op.
func NewKeepSet(enabled bool) *KeepSet {
	if !enabled {
		return nil
	}
	return &KeepSet{paths: map[string]bool{}}
}

// Add records a local file as part of the repository.
func (k *KeepSet) Add(name string) {
	if k == nil {
		return
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return
	}
	k.mu.Lock()
	k.paths[abs] = true
	k.mu.Unlock()
}

// has reports whether a local file is part of the repository.
func (k *KeepSet) has(name string) bool {
	if k == nil {
		return false
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.paths[abs]
}

// loadPruneState reads the grace bookkeeping for a pruned tree, mapping
// root-relative paths to when they were first seen unreferenced.
func loadPruneState(root string) map[string]time.Time {
	state := map[string]time.Time{}
	data, err := os.ReadFile(filepath.Join(root, PruneStateName))
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		log.WithError(err).WithField("path", root).Warn("Failed to parse prune state; starting fresh.")
		return map[string]time.Time{}
	}
	return state
}

// savePruneState persists the grace bookkeeping, or removes it when no
// files are waiting out their grace period.
func savePruneState(root string, state map[string]time.Time) {
	name := filepath.Join(root, PruneStateName)
	if len(state) == 0 {
		_ = os.Remove(name)
		return
	}
	data, err := json.Marshal(state)
	if err == nil {
		err = os.WriteFile(name, data, 0644)
	}
	if err != nil {
		log.WithError(err).WithField("path", name).Warn("Failed to save prune state.")
	}
}

// PruneTree removes files under root that are not part of the current sync,
// then clears out any directories left empty. A grace period defers
// deletion until a file has been unreferenced across runs for that long,
// so clients holding earlier metadata keep working. Failures are logged
// and do not fail the sync.
func PruneTree(root string, keep *KeepSet, grace time.Duration, dryRun bool) {
	if keep == nil {
		return
	}
	var state map[string]time.Time
	if grace > 0 {
		state = loadPruneState(root)
	}
	now := time.Now()

	var dirs []string
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name != root {
				dirs = append(dirs, name)
			}
			return nil
		}
		base := filepath.Base(name)
		if base == PruneStateName || base == MissingStateName ||
			base == DiscoverStateName || base == LockFileName {
			return nil
		}
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return nil
		}
		if keep.has(name) {
			delete(state, rel)
			return nil
		}

		// Internal work files are removed regardless of grace; anything
		// else waits out the grace period across runs first.
		internal := strings.HasSuffix(base, StagedSuffix) || strings.HasSuffix(base, PartialSuffix)
		if grace > 0 && !internal {
			first, ok := state[rel]
			if !ok {
				// A dry run records nothing, so report the pending grace
				// entry instead of staying silent about the file.
				if dryRun {
					log.WithField("path", name).Info("Stale file would enter the prune grace period.")
					return nil
				}
				state[rel] = now
				log.WithField("path", name).Debug("Stale file entered the prune grace period.")
				return nil
			}
			if now.Sub(first) < grace {
				return nil
			}
		}
		if dryRun {
			Stats.Pruned.Add(1)
			log.WithField("path", name).Info("Would prune stale file.")
			return nil
		}
		if err := os.Remove(name); err != nil {
			log.WithError(err).WithField("path", name).Warn("Failed to prune stale file.")
			return nil
		}
		delete(state, rel)
		Stats.Pruned.Add(1)
		log.WithField("path", name).Debug("Pruned stale file.")
		return nil
	})
	if err != nil {
		log.WithError(err).WithField("path", root).Warn("Failed to walk repository for pruning.")
	}

	// Drop bookkeeping for files that no longer exist, then persist it.
	if grace > 0 && !dryRun {
		for rel := range state {
			if _, err := os.Stat(filepath.Join(root, rel)); errors.Is(err, fs.ErrNotExist) {
				delete(state, rel)
			}
		}
		savePruneState(root, state)
	}
	if dryRun {
		return
	}

	// Remove now-empty directories deepest first. Separator count is the
	// true depth, so a parent is always removed after its children even
	// when a sibling's child has a longer name.
	sep := string(os.PathSeparator)
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], sep) > strings.Count(dirs[j], sep)
	})
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}

// Decompressor wraps a reader with the decompressor matching the file name
// extension, returning an optional close function for the wrapper. Staged
// suffixes are ignored so staged indexes can be parsed before promotion.
func Decompressor(r io.Reader, filename string) (io.Reader, func(), error) {
	filename = strings.TrimSuffix(filename, StagedSuffix)
	switch {
	case strings.HasSuffix(filename, ".gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return gz, func() { _ = gz.Close() }, nil
	case strings.HasSuffix(filename, ".bz2"):
		return bzip2.NewReader(r), nil, nil
	case strings.HasSuffix(filename, ".xz"):
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return xr, nil, nil
	case strings.HasSuffix(filename, ".zst"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return zr, zr.Close, nil
	default:
		return r, nil, nil
	}
}

// SniffDecompressor wraps a reader with the decompressor matching its
// leading magic bytes, for formats whose file names carry no extension.
func SniffDecompressor(r io.Reader) (io.Reader, func(), error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(6)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return gz, func() { _ = gz.Close() }, nil
	case len(magic) >= 3 && magic[0] == 'B' && magic[1] == 'Z' && magic[2] == 'h':
		return bzip2.NewReader(br), nil, nil
	case len(magic) >= 6 && bytes.Equal(magic[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		xr, err := xz.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return xr, nil, nil
	case len(magic) >= 4 && bytes.Equal(magic[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return zr, zr.Close, nil
	default:
		return br, nil, nil
	}
}

// LocalJoin resolves a repository-relative path under root, rejecting paths
// that would escape the destination tree.
func LocalJoin(root, rel string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	dst := filepath.Join(root, filepath.FromSlash(clean))
	rootClean := filepath.Clean(root)
	prefix := rootClean + string(os.PathSeparator)
	if rootClean == string(os.PathSeparator) {
		prefix = rootClean
	}
	if dst != rootClean && !strings.HasPrefix(dst, prefix) {
		return "", fmt.Errorf("path %q escapes destination %q", rel, root)
	}
	return dst, nil
}
