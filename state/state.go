// Package state persists the mirror server's per-resource crawl tracking
// so discovered repositories survive restarts.
package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"gopkg.in/yaml.v3"
)

// Entry tracks one resource: a discovered or configured repository, or a
// generic cached file.
type Entry struct {
	// Kind is the repository format, or "generic" for plain files.
	Kind string `yaml:"kind"`
	// Path is the request path of the resource (a deb entry uses the
	// suite directory).
	Path string `yaml:"path"`
	// Root is the request path prefix covering every file the resource
	// owns; requests below it keep the resource alive.
	Root                string    `yaml:"root,omitempty"`
	LastRequested       time.Time `yaml:"last_requested,omitempty"`
	LastCrawled         time.Time `yaml:"last_crawled,omitempty"`
	NextCrawl           time.Time `yaml:"next_crawl,omitempty"`
	LastSeenInInventory time.Time `yaml:"last_seen_in_inventory,omitempty"`
	LastError           string    `yaml:"last_error,omitempty"`
}

// file is the on-disk YAML layout.
type file struct {
	Entries map[string]*Entry `yaml:"entries"`
}

// Store is the in-memory state with dirty tracking for periodic flushes.
type Store struct {
	mu    sync.Mutex
	file  file
	dirty bool
}

// S is the active store, installed by Load.
var S *Store

// Load reads the state file, treating a missing or empty file as a fresh
// store. Other read failures abort startup so a state file the process
// cannot read is never silently replaced.
func Load() error {
	s := &Store{file: file{Entries: map[string]*Entry{}}}
	name := cfg.C.StatePath
	data, err := os.ReadFile(name)
	switch {
	case err == nil && len(data) > 0:
		if err := yaml.Unmarshal(data, &s.file); err != nil {
			return err
		}
		// A null or absent entries mapping unmarshals over the initialized
		// map with a nil one, so the map is restored before any write.
		if s.file.Entries == nil {
			s.file.Entries = map[string]*Entry{}
		}
		// Drop null entries a hand-edited file may carry, as lookups
		// dereference the stored pointer.
		for key, e := range s.file.Entries {
			if e == nil {
				delete(s.file.Entries, key)
			}
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read state %s: %w", name, err)
	}
	S = s
	return nil
}

// MarkRequested records a client request for a resource, creating it if
// needed. It returns a copy of the entry and whether the resource was
// already tracked, so a caller can tell a registration this request created
// from one an earlier request established.
func (s *Store) MarkRequested(kind, key, path, root string, now time.Time) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, tracked := s.file.Entries[key]
	if !tracked {
		e = &Entry{}
		s.file.Entries[key] = e
	}
	e.Kind = kind
	e.Path = path
	e.Root = root
	e.LastRequested = now
	s.dirty = true
	return *e, tracked
}

// MarkSeenInInventory records that a configured mount still advertises the
// resource, shielding it from eviction.
func (s *Store) MarkSeenInInventory(kind, key, path, root string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.file.Entries[key]
	if e == nil {
		e = &Entry{}
		s.file.Entries[key] = e
	}
	e.Kind = kind
	e.Path = path
	e.Root = root
	e.LastSeenInInventory = now
	s.dirty = true
}

// MarkCrawled records a crawl outcome, scheduling the next attempt sooner
// after failures.
func (s *Store) MarkCrawled(key string, now time.Time, crawlErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.file.Entries[key]
	if e == nil {
		return
	}
	e.LastCrawled = now
	if crawlErr != nil {
		e.LastError = crawlErr.Error()
		e.NextCrawl = now.Add(cfg.C.FailureRetryInterval)
	} else {
		e.LastError = ""
		e.NextCrawl = now.Add(cfg.C.RepoCrawlInterval)
	}
	s.dirty = true
}

// Delete removes a resource from tracking.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.file.Entries[key]; ok {
		delete(s.file.Entries, key)
		s.dirty = true
	}
}

// Entry returns a copy of a tracked resource.
func (s *Store) Entry(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.file.Entries[key]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Snapshot returns a copy of every tracked resource.
func (s *Store) Snapshot() map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Entry, len(s.file.Entries))
	for key, e := range s.file.Entries {
		out[key] = *e
	}
	return out
}

// TouchRepoMembers refreshes the last-requested time of every repository
// whose root covers a request path, keeping repositories alive while their
// files are fetched. It reports whether any repository matched.
func (s *Store) TouchRepoMembers(reqPath string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := false
	for _, e := range s.file.Entries {
		if e.Kind == "generic" || e.Root == "" {
			continue
		}
		under := reqPath == e.Root || e.Root == "/" ||
			(len(reqPath) > len(e.Root) && reqPath[:len(e.Root)] == e.Root && reqPath[len(e.Root)] == '/')
		if !under {
			continue
		}
		e.LastRequested = now
		matched = true
		s.dirty = true
	}
	return matched
}

// Save writes the state atomically when it has changed.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := yaml.Marshal(s.file)
	if err != nil {
		return err
	}
	name := cfg.C.StatePath
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// FlushLoop saves periodically and once more at shutdown.
func (s *Store) FlushLoop(ctx context.Context) {
	t := time.NewTicker(cfg.C.StateFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = s.Save()
			return
		case <-t.C:
			_ = s.Save()
			// Re-arm from the active config so a reload takes effect.
			t.Reset(cfg.C.StateFlushInterval)
		}
	}
}
