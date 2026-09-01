package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// debFile is one file referenced by repository metadata with its expected
// size and checksums, keyed by lowercase algorithm name.
type debFile struct {
	path string
	size int64
	sums map[string]string
}

// expect converts the file's metadata into a download expectation.
func (f *debFile) expect() *fetch.Expect {
	e := &fetch.Expect{Size: f.size, Sums: f.sums}
	if f.size <= 0 {
		e.Size = -1
	}
	return e
}

// debRelease is the parsed subset of an apt Release file needed to mirror
// a suite.
type debRelease struct {
	components    []string
	architectures []string
	acquireByHash bool
	files         map[string]*debFile
}

// debReleaseSet tracks one staged set of apt release documents.
type debReleaseSet struct {
	names         []string
	dsts          map[string]string
	missing       map[string]bool
	data          []byte
	authenticated bool
}

// cleanup removes unpublished release files left by a failed synchronization.
func (r *debReleaseSet) cleanup() {
	for _, dst := range r.dsts {
		_ = os.Remove(dst + fetch.StagedSuffix)
	}
}

// stageDebRelease stages and verifies the apt release documents before any
// checksums or package paths are trusted.
func stageDebRelease(ctx context.Context, src *fetch.Source, destSuite, suitePrefix string, opts *Options) (*debReleaseSet, error) {
	r := &debReleaseSet{
		names:   []string{"InRelease", "Release", "Release.gpg"},
		dsts:    map[string]string{},
		missing: map[string]bool{},
	}
	for _, name := range r.names {
		dst, err := fetch.LocalJoin(destSuite, name)
		if err != nil {
			return nil, err
		}
		r.dsts[name] = dst
		if err := r.fetch(ctx, src, suitePrefix, name, false); err != nil {
			r.cleanup()
			return nil, err
		}
	}

	mode := opts.SignatureMode
	if mode == "" {
		mode = SignatureOff
	}
	if mode == SignatureOff {
		return r.selectData(false)
	}
	verifier, err := newSignatureVerifier(opts)
	if err != nil {
		r.cleanup()
		return nil, err
	}

	// InRelease is self-contained, so a mismatch only needs that document
	// refetched. Detached Release signatures retry both members together.
	if !r.missing["InRelease"] {
		for attempt := 0; attempt < 2; attempt++ {
			data, err := os.ReadFile(fetch.StagedOrFinal(r.dsts["InRelease"]))
			if err != nil {
				r.cleanup()
				return nil, err
			}
			plaintext, fingerprint, verifyErr := verifier.verifyClearsigned(ctx, data)
			if verifyErr == nil {
				r.data = plaintext
				r.authenticated = true
				log.WithField("fingerprint", fingerprint).Debug("Verified InRelease signature.")
				break
			}
			if attempt == 1 {
				r.cleanup()
				return nil, fmt.Errorf("verify InRelease signature after refetch: %w", verifyErr)
			}
			log.WithError(verifyErr).Warn("Staged InRelease signature failed verification; refetching.")
			if err := r.fetch(ctx, src, suitePrefix, "InRelease", true); err != nil {
				r.cleanup()
				return nil, err
			}
			if r.missing["InRelease"] {
				break
			}
		}
	}

	if !r.missing["Release.gpg"] {
		if r.missing["Release"] {
			r.cleanup()
			return nil, errors.New("repository serves Release.gpg without Release")
		}
		for attempt := 0; attempt < 2; attempt++ {
			fingerprint, verifyErr := verifier.verifyDetached(ctx, fetch.StagedOrFinal(r.dsts["Release"]), fetch.StagedOrFinal(r.dsts["Release.gpg"]), "")
			if verifyErr == nil {
				log.WithField("fingerprint", fingerprint).Debug("Verified Release.gpg signature.")
				if r.missing["InRelease"] {
					r.authenticated = true
				}
				break
			}
			if attempt == 1 {
				r.cleanup()
				return nil, fmt.Errorf("verify Release.gpg signature after refetch: %w", verifyErr)
			}
			log.WithError(verifyErr).Warn("Staged Release signature failed verification; refetching the pair.")
			for _, name := range []string{"Release", "Release.gpg"} {
				if err := r.fetch(ctx, src, suitePrefix, name, true); err != nil {
					r.cleanup()
					return nil, err
				}
			}
			if r.missing["Release"] {
				r.cleanup()
				return nil, errors.New("release disappeared while refetching its signature pair")
			}
			if r.missing["Release.gpg"] {
				break
			}
		}
	}

	if r.missing["InRelease"] {
		if r.missing["Release"] {
			r.cleanup()
			return nil, errors.New("repository serves neither InRelease nor Release")
		}
		if r.missing["Release.gpg"] && mode == SignatureRequired {
			r.cleanup()
			return nil, errors.New("repository signature is required but Release.gpg is missing")
		}
		data, err := os.ReadFile(fetch.StagedOrFinal(r.dsts["Release"]))
		if err != nil {
			r.cleanup()
			return nil, err
		}
		r.data = data
	}
	return r, nil
}

func (r *debReleaseSet) fetch(ctx context.Context, src *fetch.Source, suitePrefix, name string, fresh bool) error {
	var err error
	if fresh {
		_, err = fetch.FileFresh(ctx, src, suitePrefix+name, r.dsts[name], nil, true)
	} else {
		_, err = fetch.File(ctx, src, suitePrefix+name, r.dsts[name], nil, true, false)
	}
	switch {
	case err == nil:
		r.missing[name] = false
		return nil
	case errors.Is(err, fetch.ErrNotFound), errors.Is(err, fetch.ErrForbidden):
		r.missing[name] = true
		return nil
	default:
		return fmt.Errorf("fetch %s: %w", name, err)
	}
}

func (r *debReleaseSet) selectData(authenticated bool) (*debReleaseSet, error) {
	name := "InRelease"
	if r.missing[name] {
		name = "Release"
	}
	if r.missing[name] {
		r.cleanup()
		return nil, errors.New("repository serves neither InRelease nor Release")
	}
	data, err := os.ReadFile(fetch.StagedOrFinal(r.dsts[name]))
	if err != nil {
		r.cleanup()
		return nil, err
	}
	r.data = data
	r.authenticated = authenticated
	return r, nil
}

// releaseSumFields maps Release checksum block names to algorithm names,
// including the MD5sum casing some repositories use.
var releaseSumFields = map[string]string{
	"MD5Sum": "md5sum",
	"MD5sum": "md5sum",
	"SHA1":   "sha1",
	"SHA256": "sha256",
	"SHA512": "sha512",
}

// byHashDirs maps checksum algorithm names to the by-hash directory names
// apt uses.
var byHashDirs = map[string]string{
	"md5sum": "MD5Sum",
	"sha1":   "SHA1",
	"sha256": "SHA256",
	"sha512": "SHA512",
}

// parseRelease extracts the fields the sync needs from a Release or
// InRelease document.
func parseRelease(data []byte) (*debRelease, error) {
	st, err := newControlReader(bytes.NewReader(stripClearsign(data))).readStanza()
	if err != nil {
		return nil, fmt.Errorf("parse release file: %w", err)
	}
	rel := &debRelease{files: map[string]*debFile{}}
	rel.components = strings.Fields(st.Get("Components"))
	rel.architectures = strings.Fields(st.Get("Architectures"))
	rel.acquireByHash = strings.EqualFold(st.Get("Acquire-By-Hash"), "yes")

	// Merge each checksum block into one record per file path.
	for field, algo := range releaseSumFields {
		for _, line := range strings.Split(st.Get(field), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 3 {
				continue
			}
			size, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				continue
			}
			f := rel.files[parts[2]]
			if f == nil {
				f = &debFile{path: parts[2], sums: map[string]string{}}
				rel.files[parts[2]] = f
			}
			f.size = size
			f.sums[algo] = strings.ToLower(parts[0])
		}
	}
	return rel, nil
}

// syncDeb synchronizes one apt suite or flat repository from repoURL.
func syncDeb(ctx context.Context, src *fetch.Source, repoURL string, opts *Options) error {
	keep := fetch.NewKeepSet(opts.Prune)
	tr := newTrace(opts)
	ctx = tr.track(ctx)

	// Split the URL into the archive root and the suite path. Repositories
	// without a dists directory are flat, with everything relative to the
	// repository URL itself.
	u, err := url.Parse(repoURL)
	if err != nil {
		return err
	}
	suiteRel := ""
	rootURL := repoURL
	if idx := strings.Index(u.Path, "/dists/"); idx >= 0 {
		suiteRel = strings.Trim(u.Path[idx+1:], "/")
		root := *u
		root.Path = u.Path[:idx]
		rootURL = root.String()
		src.Rebase(suiteRel)
	}
	destRoot, err := opts.repoDest(rootURL)
	if err != nil {
		return err
	}
	destSuite := destRoot
	suitePrefix := ""
	if suiteRel != "" {
		if destSuite, err = fetch.LocalJoin(destRoot, suiteRel); err != nil {
			return err
		}
		suitePrefix = suiteRel + "/"
	}

	// Track the pool files this suite lists but the upstream does not serve.
	// The records live in the suite directory, as the pool is shared with
	// the archive's other suites.
	miss := opts.newMissing(destSuite)

	// Authenticate release checksums before selecting any index or pool file.
	releaseSet, err := stageDebRelease(ctx, src, destSuite, suitePrefix, opts)
	if err != nil {
		return err
	}
	defer releaseSet.cleanup()
	rel, err := parseRelease(releaseSet.data)
	if err != nil {
		return err
	}

	// Stage every index file the release lists, applying component and
	// architecture filters. Missing variants are normal; repositories list
	// checksums for files they do not serve. A dry run only fetches the
	// package and source indexes needed to plan the pool.
	var indexJobs, plannedJobs []fetch.Job
	for _, f := range sortedFiles(rel.files) {
		// Releases commonly list their own release files; those are
		// already staged above and must not be fetched twice.
		if slices.Contains(releaseSet.names, f.path) {
			continue
		}
		if !includeIndexFile(f.path, opts.Components, opts.Architectures) {
			continue
		}
		dst, err := fetch.LocalJoin(destSuite, f.path)
		if err != nil {
			return err
		}
		job := fetch.Job{
			ReqPath:  suitePrefix + f.path,
			Dst:      dst,
			Want:     f.expect(),
			Optional: true,
			Stage:    true,
		}
		if opts.DryRun {
			base, _ := splitCompressExt(f.path)
			if name := path.Base(base); name != "Packages" && name != "Sources" {
				plannedJobs = append(plannedJobs, job)
				continue
			}
		}
		indexJobs = append(indexJobs, job)
	}
	verifyFiles := opts.Verify || releaseSet.authenticated
	fetch.PlanJobs(plannedJobs, verifyFiles, keep)
	states, err := fetch.Many(ctx, src, indexJobs, opts.Workers, verifyFiles, keep, nil)
	if err != nil {
		return fmt.Errorf("fetch suite indexes: %w", err)
	}

	// Pick one local compression variant of each package and source index
	// to parse for pool file references.
	variants := map[string]string{}
	variantExt := map[string]string{}
	prefOrder := map[string]int{".gz": 0, ".xz": 1, ".zst": 2, ".bz2": 3, "": 4}
	for i, job := range indexJobs {
		if states[i] == fetch.FileMissing {
			continue
		}
		relPath := strings.TrimPrefix(job.ReqPath, suitePrefix)
		base, ext := splitCompressExt(relPath)
		if name := path.Base(base); name != "Packages" && name != "Sources" {
			continue
		}
		local := job.Dst
		if states[i] == fetch.FileStaged {
			local = job.Dst + fetch.StagedSuffix
		}
		if cur, ok := variantExt[base]; !ok || prefOrder[ext] < prefOrder[cur] {
			variantExt[base] = ext
			variants[base] = local
		}
	}

	// Collect the pool files referenced by the chosen indexes.
	pool := map[string]*debFile{}
	for _, base := range sortedKeys(variants) {
		files, err := parseIndexFile(variants[base], path.Base(base))
		if err != nil {
			return fmt.Errorf("parse %s: %w", base, err)
		}
		for _, f := range files {
			if _, ok := pool[f.path]; !ok {
				pool[f.path] = f
			}
		}
	}

	// Download pool files relative to the archive root.
	poolJobs := make([]fetch.Job, 0, len(pool))
	for _, f := range sortedFiles(pool) {
		dst, err := fetch.LocalJoin(destRoot, f.path)
		if err != nil {
			return err
		}
		poolJobs = append(poolJobs, fetch.Job{ReqPath: f.path, Dst: dst, Want: f.expect()})
	}
	log.WithField("files", len(poolJobs)).Info("Synchronizing pool files.")
	if opts.DryRun {
		fetch.PlanJobs(poolJobs, verifyFiles, keep)
	} else if _, err := fetch.Many(ctx, src, poolJobs, opts.Workers, verifyFiles, keep, miss); err != nil {
		return fmt.Errorf("fetch pool files: %w", err)
	}
	if releaseSet.authenticated {
		if err := miss.Finish(); err != nil {
			return err
		}
	}

	// Promote the staged indexes now the pool files they reference exist.
	for i, job := range indexJobs {
		if states[i] == fetch.FileMissing {
			continue
		}
		if err := fetch.PromoteOrDiscard(job.Dst, opts.DryRun); err != nil {
			return err
		}
		if rel.acquireByHash && !opts.DryRun {
			addByHash(job.Dst, strings.TrimPrefix(job.ReqPath, suitePrefix), rel, keep)
		}
	}

	// Promote the release files last to complete the metadata chain.
	for _, name := range []string{"Release.gpg", "Release", "InRelease"} {
		dst := releaseSet.dsts[name]
		if releaseSet.missing[name] {
			if !opts.DryRun {
				fetch.RemoveStale(dst)
			}
			continue
		}
		if err := fetch.PromoteOrDiscard(dst, opts.DryRun); err != nil {
			return err
		}
		if _, err := os.Stat(dst); err == nil {
			keep.Add(dst)
		}
	}

	// Publish the traces at the archive root, where the convention places
	// them and where they describe the archive as a whole rather than one
	// suite. Writing before pruning lets the keep set cover them, which
	// matters for a flat repository, whose root is itself the pruned tree.
	tr.publish(ctx, destRoot, src, keep, opts)

	// Prune only the suite directory for structured repositories; the pool
	// can be shared with other suites of the same archive.
	if opts.Prune {
		if suiteRel == "" {
			fetch.PruneTree(destRoot, keep, opts.PruneGrace, opts.DryRun)
		} else {
			log.Info("Pruning is limited to the suite directory; shared pool files are kept.")
			fetch.PruneTree(destSuite, keep, opts.PruneGrace, opts.DryRun)
		}
	}

	if releaseSet.authenticated {
		return nil
	}
	return miss.Finish()
}

// addByHash links an index file into its directory's by-hash tree for each
// checksum the release lists, matching apt's Acquire-By-Hash layout.
// Failures are logged and do not fail the sync.
func addByHash(dst, relPath string, rel *debRelease, keep *fetch.KeepSet) {
	f := rel.files[relPath]
	if f == nil || !strings.Contains(relPath, "/") {
		return
	}
	dir := filepath.Dir(dst)
	for algo, digest := range f.sums {
		sub, ok := byHashDirs[algo]
		if !ok {
			continue
		}
		link := filepath.Join(dir, "by-hash", sub, digest)
		if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
			log.WithError(err).WithField("path", link).Warn("Failed to create by-hash directory.")
			return
		}
		if _, err := os.Stat(link); err == nil {
			keep.Add(link)
			continue
		}
		if err := os.Link(dst, link); err != nil {
			// Fall back to copying when hard links are not supported.
			if err := copyFile(dst, link); err != nil {
				log.WithError(err).WithField("path", link).Warn("Failed to create by-hash entry.")
				continue
			}
		}
		keep.Add(link)
	}
}

// copyFile duplicates src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// parseIndexFile reads a Packages or Sources index and returns the pool
// files it references, with paths relative to the archive root.
func parseIndexFile(filename, kind string) ([]*debFile, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r, closeFn, err := fetch.Decompressor(f, filename)
	if err != nil {
		return nil, err
	}
	if closeFn != nil {
		defer closeFn()
	}

	cr := newControlReader(r)
	var out []*debFile
	for {
		st, err := cr.readStanza()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if kind == "Sources" {
			out = append(out, sourceFiles(st)...)
			continue
		}
		name := st.Get("Filename")
		if name == "" {
			continue
		}
		size, _ := strconv.ParseInt(st.Get("Size"), 10, 64)
		sums := map[string]string{}
		if v := st.Get("MD5sum", "MD5Sum"); v != "" {
			sums["md5sum"] = strings.ToLower(v)
		}
		for _, algo := range []string{"SHA1", "SHA256", "SHA512"} {
			if v := st.Get(algo); v != "" {
				sums[strings.ToLower(algo)] = strings.ToLower(v)
			}
		}
		out = append(out, &debFile{path: path.Clean(name), size: size, sums: sums})
	}
	return out, nil
}

// sourceSumFields maps source stanza checksum list fields to algorithm
// names; Files is the historical MD5 list.
var sourceSumFields = map[string]string{
	"Files":            "md5sum",
	"Checksums-Sha1":   "sha1",
	"Checksums-Sha256": "sha256",
	"Checksums-Sha512": "sha512",
}

// sourceFiles extracts the files of one source package stanza, merging the
// per-algorithm checksum lists by file name.
func sourceFiles(st stanza) []*debFile {
	dir := st.Get("Directory")
	byName := map[string]*debFile{}
	var order []string
	for field, algo := range sourceSumFields {
		for _, line := range strings.Split(st.Get(field), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 3 {
				continue
			}
			size, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				continue
			}
			name := parts[2]
			f := byName[name]
			if f == nil {
				f = &debFile{path: path.Join(dir, name), sums: map[string]string{}}
				byName[name] = f
				order = append(order, name)
			}
			f.size = size
			f.sums[algo] = strings.ToLower(parts[0])
		}
	}
	out := make([]*debFile, 0, len(byName))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// includeIndexFile applies the component and architecture filters to a
// release-listed file path.
func includeIndexFile(p string, comps, archs []string) bool {
	if len(comps) > 0 {
		if segs := strings.Split(p, "/"); len(segs) > 1 && !slices.Contains(comps, segs[0]) {
			return false
		}
	}
	if len(archs) > 0 {
		if arch := indexArch(p); arch != "" && !slices.Contains(archs, arch) {
			return false
		}
	}
	return true
}

// indexArch extracts the architecture a release-listed file applies to, or
// an empty string when the file is architecture independent.
func indexArch(p string) string {
	for _, seg := range strings.Split(p, "/") {
		if arch, ok := strings.CutPrefix(seg, "binary-"); ok {
			return arch
		}
		if arch, ok := strings.CutPrefix(seg, "installer-"); ok {
			return arch
		}
		if seg == "source" {
			return "source"
		}
	}
	base, _ := splitCompressExt(path.Base(p))
	if arch, ok := strings.CutPrefix(base, "Contents-udeb-"); ok {
		return arch
	}
	if arch, ok := strings.CutPrefix(base, "Contents-"); ok {
		return arch
	}
	return ""
}

// splitCompressExt splits a known compression extension off a path.
func splitCompressExt(p string) (string, string) {
	for _, ext := range []string{".gz", ".xz", ".bz2", ".zst"} {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext), ext
		}
	}
	return p, ""
}

// sortedFiles returns metadata file records in a stable path order.
func sortedFiles(files map[string]*debFile) []*debFile {
	out := make([]*debFile, 0, len(files))
	for _, f := range files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
