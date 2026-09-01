package mirror

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

const (
	rpmRepomdPath = "repodata/repomd.xml"
	rpmSigPath    = "repodata/repomd.xml.asc"
	rpmKeyPath    = "repodata/repomd.xml.key"
)

// rpmRoot is one staged repomd generation and its optional signature
// material.
type rpmRoot struct {
	repomdDst     string
	sigDst        string
	keyDst        string
	sigMissing    bool
	keyMissing    bool
	authenticated bool
}

// rpmPublishedFile is a rollback copy of one live root metadata file.
type rpmPublishedFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

// rpmWithdrawnPackage is a stale package hidden until root publication
// succeeds or restored if publication fails.
type rpmWithdrawnPackage struct {
	path   string
	backup string
}

// cleanup removes unpublished files left from staging a repomd generation.
func (r *rpmRoot) cleanup() {
	for _, name := range []string{r.repomdDst, r.sigDst, r.keyDst} {
		_ = os.Remove(name + fetch.StagedSuffix)
	}
}

// stageRPMRoot stages and optionally verifies repomd.xml with its detached
// signature. A failed check refetches the complete set once because an
// upstream rotation can occur between the requests.
func stageRPMRoot(ctx context.Context, src *fetch.Source, destDir string, opts *Options) (*rpmRoot, error) {
	r := &rpmRoot{}
	var err error
	if r.repomdDst, err = fetch.LocalJoin(destDir, rpmRepomdPath); err != nil {
		return nil, err
	}
	if r.sigDst, err = fetch.LocalJoin(destDir, rpmSigPath); err != nil {
		return nil, err
	}
	if r.keyDst, err = fetch.LocalJoin(destDir, rpmKeyPath); err != nil {
		return nil, err
	}

	mode := opts.SignatureMode
	if mode == "" {
		mode = SignatureOff
	}
	var verifier *signatureVerifier
	if mode != SignatureOff {
		verifier, err = newSignatureVerifier(opts)
		if err != nil {
			return nil, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		fresh := attempt > 0
		file := fetch.File
		if fresh {
			file = func(ctx context.Context, src *fetch.Source, reqPath, dst string, want *fetch.Expect, stage, _ bool) (fetch.FileState, error) {
				return fetch.FileFresh(ctx, src, reqPath, dst, want, stage)
			}
		}
		if _, err := file(ctx, src, rpmRepomdPath, r.repomdDst, nil, true, false); err != nil {
			r.cleanup()
			return nil, fmt.Errorf("fetch repomd.xml: %w", err)
		}

		r.sigMissing = false
		r.keyMissing = false
		for _, extra := range []struct {
			req     string
			dst     string
			missing *bool
		}{
			{rpmSigPath, r.sigDst, &r.sigMissing},
			{rpmKeyPath, r.keyDst, &r.keyMissing},
		} {
			_, err := file(ctx, src, extra.req, extra.dst, nil, true, false)
			switch {
			case err == nil:
			case errors.Is(err, fetch.ErrNotFound), errors.Is(err, fetch.ErrForbidden):
				*extra.missing = true
			case err != nil:
				r.cleanup()
				return nil, fmt.Errorf("fetch %s: %w", extra.req, err)
			}
		}

		if mode == SignatureOff {
			return r, nil
		}
		if r.sigMissing {
			if mode == SignatureRequired {
				r.cleanup()
				return nil, errors.New("repository signature is required but repomd.xml.asc is missing")
			}
			return r, nil
		}

		keyPath := ""
		if !r.keyMissing {
			keyPath = fetch.StagedOrFinal(r.keyDst)
		}
		fingerprint, verifyErr := verifier.verifyDetached(
			ctx,
			fetch.StagedOrFinal(r.repomdDst),
			fetch.StagedOrFinal(r.sigDst),
			keyPath,
		)
		if verifyErr == nil {
			r.authenticated = true
			log.WithField("fingerprint", fingerprint).Debug("Verified repomd.xml signature.")
			return r, nil
		}
		if !fresh {
			log.WithError(verifyErr).Warn("Staged repomd.xml signature failed verification; refetching the pair.")
			continue
		}
		r.cleanup()
		return nil, fmt.Errorf("verify repomd.xml signature after refetch: %w", verifyErr)
	}
	return nil, errors.New("unable to stage repomd.xml")
}

// snapshotRPMRoot reads the current live root set before publication.
func snapshotRPMRoot(paths ...string) ([]rpmPublishedFile, error) {
	files := make([]rpmPublishedFile, 0, len(paths))
	for _, name := range paths {
		file := rpmPublishedFile{path: name}
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			files = append(files, file)
			continue
		}
		if err != nil {
			return nil, err
		}
		file.data, err = os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		file.mode = info.Mode()
		file.exists = true
		files = append(files, file)
	}
	return files, nil
}

// restoreRPMRoot replaces a partially published root with its prior files.
func restoreRPMRoot(files []rpmPublishedFile) error {
	var errs []error
	for _, file := range files {
		if !file.exists {
			if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		tmp := file.path + ".rollback" + fetch.StagedSuffix
		if err := os.WriteFile(tmp, file.data, file.mode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(tmp, file.path); err != nil {
			_ = os.Remove(tmp)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// withdrawMissingPackages hides checksum-invalid packages the authenticated
// root no longer provides upstream.
func withdrawMissingPackages(jobs []fetch.Job, states []fetch.FileState) ([]rpmWithdrawnPackage, error) {
	var withdrawn []rpmWithdrawnPackage
	for i, state := range states {
		if state != fetch.FileMissing {
			continue
		}
		if _, err := os.Stat(jobs[i].Dst); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return withdrawn, err
		}
		backup := jobs[i].Dst + ".withdrawn" + fetch.StagedSuffix
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			return withdrawn, err
		}
		if err := os.Rename(jobs[i].Dst, backup); err != nil {
			return withdrawn, err
		}
		withdrawn = append(withdrawn, rpmWithdrawnPackage{path: jobs[i].Dst, backup: backup})
	}
	return withdrawn, nil
}

// finishWithdrawnPackages removes hidden packages after publication or puts
// them back when publication fails.
func finishWithdrawnPackages(files []rpmWithdrawnPackage, published bool) error {
	var errs []error
	for _, file := range files {
		if published {
			if err := os.Remove(file.backup); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.Rename(file.backup, file.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// publishRPMRoot publishes signature material immediately before repomd.xml.
// Missing optional files are removed only after the replacement is ready.
func publishRPMRoot(r *rpmRoot, keep *fetch.KeepSet, dryRun bool) (retErr error) {
	var published []rpmPublishedFile
	if !dryRun {
		var err error
		published, err = snapshotRPMRoot(r.keyDst, r.sigDst, r.repomdDst)
		if err != nil {
			return err
		}
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, restoreRPMRoot(published))
			}
		}()
	}
	for _, extra := range []struct {
		dst     string
		missing bool
	}{
		{r.keyDst, r.keyMissing},
		{r.sigDst, r.sigMissing},
	} {
		if extra.missing {
			if !dryRun {
				if err := os.Remove(extra.dst); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			continue
		}
		if err := fetch.PromoteOrDiscard(extra.dst, dryRun); err != nil {
			return err
		}
		if _, err := os.Stat(extra.dst); err == nil {
			keep.Add(extra.dst)
		}
	}
	if err := fetch.PromoteOrDiscard(r.repomdDst, dryRun); err != nil {
		return err
	}
	keep.Add(r.repomdDst)
	return nil
}

// repoMD is the partial XML schema for a yum repomd.xml file. Only the
// fields the sync needs are decoded; unknown elements are ignored so
// upstream additions do not break parsing.
type repoMD struct {
	Data []repoData `xml:"data"`
}

// repoData is one metadata file referenced from repomd.xml.
type repoData struct {
	Type     string       `xml:"type,attr"`
	Checksum repoChecksum `xml:"checksum"`
	Location repoLocation `xml:"location"`
	Size     int64        `xml:"size"`
}

// repoChecksum is a checksum element with its algorithm type.
type repoChecksum struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// repoLocation captures the relative href of a metadata or package file.
type repoLocation struct {
	Href string `xml:"href,attr"`
}

// rpmPackage is the minimal package element decoded from primary metadata,
// carrying the location, checksum, and size needed to mirror the file.
type rpmPackage struct {
	Checksum repoChecksum `xml:"checksum"`
	Size     rpmSize      `xml:"size"`
	Location repoLocation `xml:"location"`
}

// rpmSize carries the on-disk package size from primary metadata.
type rpmSize struct {
	Package int64 `xml:"package,attr"`
}

// syncRPM synchronizes one RPM repository from src into destDir.
func syncRPM(ctx context.Context, src *fetch.Source, destDir string, opts *Options) error {
	keep := fetch.NewKeepSet(opts.Prune)
	miss := opts.newMissing(destDir)
	tr := newTrace(opts)
	ctx = tr.track(ctx)

	// Authenticate the root metadata before using it to select any files.
	root, err := stageRPMRoot(ctx, src, destDir, opts)
	if err != nil {
		return err
	}
	defer root.cleanup()
	md, err := readRepomd(fetch.StagedOrFinal(root.repomdDst))
	if err != nil {
		return err
	}

	// Download the metadata files referenced by repomd. Their names embed
	// content hashes, so they can be written directly into place. A dry run
	// only fetches the indexes that must be parsed, staged for discard.
	var jobs, planned []fetch.Job
	var primary *repoData
	var deltaMDs []*repoData
	var parseDsts []string
	for i, data := range md.Data {
		if data.Location.Href == "" {
			continue
		}
		dst, err := fetch.LocalJoin(destDir, data.Location.Href)
		if err != nil {
			return err
		}
		job := fetch.Job{
			ReqPath: data.Location.Href,
			Dst:     dst,
			Want:    fetch.MakeExpect(data.Size, data.Checksum.Type, data.Checksum.Value),
		}
		parsed := false
		switch data.Type {
		case "primary":
			primary = &md.Data[i]
			parsed = true
		case "prestodelta", "deltainfo":
			deltaMDs = append(deltaMDs, &md.Data[i])
			parsed = true
		}
		if opts.DryRun {
			if !parsed {
				planned = append(planned, job)
				continue
			}
			job.Stage = true
			parseDsts = append(parseDsts, dst)
		}
		jobs = append(jobs, job)
	}
	verifyMetadata := opts.Verify || root.authenticated
	fetch.PlanJobs(planned, verifyMetadata, keep)
	if _, err := fetch.Many(ctx, src, jobs, opts.Workers, verifyMetadata, keep, nil); err != nil {
		return fmt.Errorf("fetch repository metadata: %w", err)
	}
	if primary == nil {
		return errors.New("repository metadata lists no primary index")
	}

	// Collect package locations from the primary index, plus any delta
	// packages referenced by prestodelta metadata.
	primaryPath, err := fetch.LocalJoin(destDir, primary.Location.Href)
	if err != nil {
		return err
	}
	pkgs, err := readPrimary(fetch.StagedOrFinal(primaryPath))
	if err != nil {
		return err
	}
	jobs = jobs[:0]
	for _, pkg := range pkgs {
		if pkg.Location.Href == "" {
			continue
		}
		dst, err := fetch.LocalJoin(destDir, pkg.Location.Href)
		if err != nil {
			return err
		}
		jobs = append(jobs, fetch.Job{
			ReqPath: pkg.Location.Href,
			Dst:     dst,
			Want:    fetch.MakeExpect(pkg.Size.Package, pkg.Checksum.Type, pkg.Checksum.Value),
		})
	}
	for _, deltaMD := range deltaMDs {
		deltaPath, err := fetch.LocalJoin(destDir, deltaMD.Location.Href)
		if err != nil {
			return err
		}
		deltas, err := readDeltas(fetch.StagedOrFinal(deltaPath))
		if err != nil {
			return err
		}
		for _, delta := range deltas {
			if delta.Filename == "" {
				continue
			}
			dst, err := fetch.LocalJoin(destDir, delta.Filename)
			if err != nil {
				return err
			}
			jobs = append(jobs, fetch.Job{
				ReqPath: delta.Filename,
				Dst:     dst,
				Want:    fetch.MakeExpect(delta.Size, delta.Checksum.Type, delta.Checksum.Value),
			})
		}
	}
	log.WithField("packages", len(jobs)).Info("Synchronizing packages.")
	verifyPackages := opts.Verify || root.authenticated
	var packageStates []fetch.FileState
	if opts.DryRun {
		fetch.PlanJobs(jobs, verifyPackages, keep)
	} else {
		packageStates, err = fetch.Many(ctx, src, jobs, opts.Workers, verifyPackages, keep, miss)
		if err != nil {
			return fmt.Errorf("fetch packages: %w", err)
		}
	}

	// A dry run discards the indexes it staged for parsing.
	for _, dst := range parseDsts {
		_ = os.Remove(dst + fetch.StagedSuffix)
	}

	// Keep the previous root live until every referenced package is present
	// under the configured missing-file policy.
	if err := miss.Finish(); err != nil {
		return err
	}
	var withdrawn []rpmWithdrawnPackage
	if root.authenticated {
		withdrawn, err = withdrawMissingPackages(jobs, packageStates)
		if err != nil {
			_ = finishWithdrawnPackages(withdrawn, false)
			return fmt.Errorf("withdraw stale packages: %w", err)
		}
	}
	// Publish the verified root only after every referenced file is ready.
	if err := publishRPMRoot(root, keep, opts.DryRun); err != nil {
		return errors.Join(err, finishWithdrawnPackages(withdrawn, false))
	}
	if err := finishWithdrawnPackages(withdrawn, true); err != nil {
		log.WithError(err).Warn("Unable to remove withdrawn package files.")
	}

	// Publish the traces, upstream's included, before pruning so the keep
	// set covers them.
	tr.publish(ctx, destDir, src, keep, opts)

	// Remove files that are no longer part of the repository.
	if opts.Prune {
		fetch.PruneTree(destDir, keep, opts.PruneGrace, opts.DryRun)
	}

	return nil
}

// rpmDelta is one delta package referenced from prestodelta metadata.
type rpmDelta struct {
	Filename string       `xml:"filename"`
	Size     int64        `xml:"size"`
	Checksum repoChecksum `xml:"checksum"`
}

// readDeltas streams prestodelta metadata and collects each delta package
// reference so drpm files are mirrored alongside full packages.
func readDeltas(filename string) ([]rpmDelta, error) {
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

	dec := xml.NewDecoder(r)
	var deltas []rpmDelta
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse delta metadata %s: %w", filename, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "delta" {
			continue
		}
		var delta rpmDelta
		if err := dec.DecodeElement(&delta, &start); err != nil {
			return nil, fmt.Errorf("parse delta entry %s: %w", filename, err)
		}
		if delta.Filename != "" {
			deltas = append(deltas, delta)
		}
	}
	return deltas, nil
}

// readRepomd decodes a repomd.xml file.
func readRepomd(filename string) (*repoMD, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var md repoMD
	if err := xml.NewDecoder(f).Decode(&md); err != nil {
		return nil, fmt.Errorf("parse repomd %s: %w", filename, err)
	}
	return &md, nil
}

// readPrimary streams a primary metadata index and collects each package
// entry without loading the whole document into memory.
func readPrimary(filename string) ([]rpmPackage, error) {
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

	dec := xml.NewDecoder(r)
	var pkgs []rpmPackage
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse primary metadata %s: %w", filename, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "package" {
			continue
		}
		var pkg rpmPackage
		if err := dec.DecodeElement(&pkg, &start); err != nil {
			return nil, fmt.Errorf("parse package entry %s: %w", filename, err)
		}
		if pkg.Location.Href != "" {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}
