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

	// Stage repomd.xml so clients of a live tree keep a consistent view of
	// the old repository until everything else is in place.
	repomdDst, err := fetch.LocalJoin(destDir, "repodata/repomd.xml")
	if err != nil {
		return err
	}
	if _, err := fetch.File(ctx, src, "repodata/repomd.xml", repomdDst, nil, true, false); err != nil {
		return fmt.Errorf("fetch repomd.xml: %w", err)
	}
	md, err := readRepomd(fetch.StagedOrFinal(repomdDst))
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
	fetch.PlanJobs(planned, opts.Verify, keep)
	if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, keep, nil); err != nil {
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
	if opts.DryRun {
		fetch.PlanJobs(jobs, opts.Verify, keep)
	} else if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, keep, miss); err != nil {
		return fmt.Errorf("fetch packages: %w", err)
	}

	// A dry run discards the indexes it staged for parsing.
	for _, dst := range parseDsts {
		_ = os.Remove(dst + fetch.StagedSuffix)
	}

	// Stage the optional signature material so it is published together
	// with the repomd.xml it signs, never beside the previous index.
	var sigDsts []string
	for _, extra := range []string{"repodata/repomd.xml.asc", "repodata/repomd.xml.key"} {
		dst, err := fetch.LocalJoin(destDir, extra)
		if err != nil {
			return err
		}
		_, err = fetch.File(ctx, src, extra, dst, nil, true, false)
		switch {
		case err == nil:
			sigDsts = append(sigDsts, dst)
		case errors.Is(err, fetch.ErrNotFound):
			// The upstream dropped the signature; drop the local copy so
			// a stale signature is never served beside a new repomd.xml.
			if !opts.DryRun {
				fetch.RemoveStale(dst)
			}
		default:
			return fmt.Errorf("fetch %s: %w", extra, err)
		}
	}

	// Promote the signatures and then repomd.xml so the published metadata
	// chain is complete.
	for _, dst := range sigDsts {
		if err := fetch.PromoteOrDiscard(dst, opts.DryRun); err != nil {
			return err
		}
		if _, err := os.Stat(dst); err == nil {
			keep.Add(dst)
		}
	}
	if err := fetch.PromoteOrDiscard(repomdDst, opts.DryRun); err != nil {
		return err
	}
	keep.Add(repomdDst)

	// Publish the traces, upstream's included, before pruning so the keep
	// set covers them.
	tr.publish(ctx, destDir, src, keep, opts)

	// Remove files that are no longer part of the repository.
	if opts.Prune {
		fetch.PruneTree(destDir, keep, opts.PruneGrace, opts.DryRun)
	}

	// The metadata is published either way; missing packages only decide
	// whether the run reports itself as failed.
	return miss.Finish()
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
