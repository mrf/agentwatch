// Package filewatch provides shared file discovery for agentwatch sources.
// It handles directory walking with glob pattern filters, age-window filtering,
// and efficient re-scanning by tracking directory modification times.
//
// Sources (claude, codex, antigravity) use Walker in their Discover() implementations.
// Keep this package internal until a custom source author demonstrably needs it.
package filewatch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is a discovered file.
type Entry struct {
	Path    string
	ModTime time.Time
}

// Option configures a Walker.
type Option func(*Walker)

// WithPattern adds a glob pattern that file names must match (e.g. "*.jsonl").
// Multiple patterns are ORed: a file matching any pattern is included.
// If no pattern is added, all files are included.
func WithPattern(pattern string) Option {
	return func(w *Walker) {
		w.patterns = append(w.patterns, pattern)
	}
}

// WithMaxAge limits discovery to files whose modification time is within d of
// the current time. A zero duration (the default) disables age filtering.
func WithMaxAge(d time.Duration) Option {
	return func(w *Walker) {
		w.maxAge = d
	}
}

// WithFollowSymlinks controls whether symlinks that point to directories are
// followed during the walk. Symlinks pointing to regular files are always
// followed. The default is false.
func WithFollowSymlinks(follow bool) Option {
	return func(w *Walker) {
		w.followSymlinks = follow
	}
}

// WithSkipDir registers a predicate called with the path of every directory
// found below a root (never the root itself). Directories for which it returns
// true are not descended into.
func WithSkipDir(skip func(path string) bool) Option {
	return func(w *Walker) {
		w.skipDir = skip
	}
}

// WithClock injects a clock function used to evaluate age windows. Intended
// for testing; production callers should omit this option.
func WithClock(clock func() time.Time) Option {
	return func(w *Walker) {
		w.clock = clock
	}
}

// Walker walks one or more directory trees and returns files that satisfy the
// configured filters. Repeated calls to Discover skip directories whose
// modification time has not changed since the previous scan.
type Walker struct {
	roots          []string
	patterns       []string
	maxAge         time.Duration
	followSymlinks bool
	clock          func() time.Time
	skipDir        func(path string) bool

	mu       sync.Mutex
	dirCache map[string]dirSnapshot
}

// dirSnapshot records the mtime, matching file list, and subdirectory paths
// for one directory. Both files and subdirs are needed so that a cache hit can
// still recurse into subdirectories without re-reading the parent directory.
type dirSnapshot struct {
	mtime   time.Time
	files   []Entry  // regular files (and file-symlinks) that matched the pattern
	subdirs []string // subdirectory paths (including followed dir-symlinks)
}

// New creates a Walker rooted at the given roots.
func New(roots []string, opts ...Option) *Walker {
	w := &Walker{
		roots:    roots,
		clock:    time.Now,
		dirCache: make(map[string]dirSnapshot),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Discover walks all configured roots and returns matching files.
// Missing root directories are silently skipped, as are unreadable or vanished
// directories below a root. An unreadable root returns its error. A cancelled
// context causes an early return with the context error.
func (w *Walker) Discover(ctx context.Context) ([]Entry, error) {
	var results []Entry
	now := w.clock()

	for _, root := range w.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := w.walkRoot(ctx, root, now)
		if err != nil {
			return nil, err
		}
		results = append(results, entries...)
	}
	return results, nil
}

func (w *Walker) walkRoot(ctx context.Context, root string, now time.Time) ([]Entry, error) {
	// Stat, not Lstat: a root that is itself a symlink to a directory is walked.
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var results []Entry
	if err := w.walkDir(ctx, root, true, now, make(map[dirID]struct{}), &results); err != nil {
		return nil, err
	}
	return results, nil
}

// walkDir recursively walks dir, appending matching files to results.
// ancestors holds the identity of every directory on the current walk path; a
// directory that is already an ancestor (a symlink loop) is not walked again.
//
// Only the root's errors are fatal (an unreadable root is a configuration
// problem). Below the root, any Stat/ReadDir failure skips that directory and
// is not cached, so it is retried on the next scan: permission errors must not
// hide sibling sessions, and a directory removed mid-walk is a normal race.
func (w *Walker) walkDir(ctx context.Context, dir string, isRoot bool, now time.Time, ancestors map[dirID]struct{}, results *[]Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Stat(dir)
	if err != nil {
		if isRoot && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	id, ok := identify(dir, info)
	if !ok {
		return nil
	}
	if _, loop := ancestors[id]; loop {
		return nil
	}
	ancestors[id] = struct{}{}
	defer delete(ancestors, id)
	dirMtime := info.ModTime()

	w.mu.Lock()
	snap, cached := w.dirCache[dir]
	w.mu.Unlock()

	if cached && snap.mtime.Equal(dirMtime) {
		// Directory contents unchanged: use the cached file list, but re-stat
		// each file. Appending to a file changes its mtime and not its
		// directory's, so a cached mtime would age an active file out of the
		// window. A file that vanished is dropped.
		for _, e := range snap.files {
			info, statErr := os.Stat(e.Path)
			if statErr != nil || !info.Mode().IsRegular() {
				continue
			}
			e.ModTime = info.ModTime()
			if w.passesAge(e.ModTime, now) {
				*results = append(*results, e)
			}
		}
		// A file added to a subdirectory only updates that subdir's mtime, not
		// this directory's mtime. Always recurse into known subdirectories so
		// changes deep in the tree are not missed.
		for _, sub := range snap.subdirs {
			if err := w.walkDir(ctx, sub, false, now, ancestors, results); err != nil {
				return err
			}
		}
		return nil
	}

	// Directory changed (or first visit): read entries and update cache.
	des, err := os.ReadDir(dir)
	if err != nil {
		if isRoot && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	var (
		dirFiles []Entry
		subdirs  []string
	)

	for _, de := range des {
		if err := ctx.Err(); err != nil {
			return err
		}

		name := de.Name()
		fullPath := filepath.Join(dir, name)
		deType := de.Type()
		isSymlink := deType&fs.ModeSymlink != 0

		if isSymlink {
			target, statErr := os.Stat(fullPath)
			if statErr != nil {
				// Broken symlink — skip.
				continue
			}
			if target.IsDir() {
				if w.followSymlinks && !w.skips(fullPath) {
					subdirs = append(subdirs, fullPath)
					if err := w.walkDir(ctx, fullPath, false, now, ancestors, results); err != nil {
						return err
					}
				}
				continue
			}
			w.collectFile(name, fullPath, target, now, &dirFiles, results)
			continue
		}

		if de.IsDir() {
			if w.skips(fullPath) {
				continue
			}
			subdirs = append(subdirs, fullPath)
			if err := w.walkDir(ctx, fullPath, false, now, ancestors, results); err != nil {
				return err
			}
			continue
		}

		fi, statErr := de.Info()
		if statErr != nil {
			continue
		}
		w.collectFile(name, fullPath, fi, now, &dirFiles, results)
	}

	w.mu.Lock()
	w.dirCache[dir] = dirSnapshot{mtime: dirMtime, files: dirFiles, subdirs: subdirs}
	w.mu.Unlock()

	return nil
}

// collectFile adds a file to the directory cache slice and, if it passes the
// age filter, to the result set. Only regular files are collected: opening a
// FIFO or device would block the poll. info must describe the file itself
// (the symlink target for symlinks).
func (w *Walker) collectFile(name, path string, info fs.FileInfo, now time.Time, dirFiles *[]Entry, results *[]Entry) {
	if !info.Mode().IsRegular() || !w.matchesPattern(name) {
		return
	}
	mtime := info.ModTime()
	e := Entry{Path: path, ModTime: mtime}
	*dirFiles = append(*dirFiles, e)
	if w.passesAge(mtime, now) {
		*results = append(*results, e)
	}
}

// skips reports whether the skip-dir hook rejects the subdirectory at path.
func (w *Walker) skips(path string) bool {
	return w.skipDir != nil && w.skipDir(path)
}

// matchesPattern reports whether name matches any of the configured patterns.
// If no patterns are configured, all names match.
func (w *Walker) matchesPattern(name string) bool {
	if len(w.patterns) == 0 {
		return true
	}
	for _, p := range w.patterns {
		matched, err := filepath.Match(p, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}

// passesAge reports whether a file with the given mtime is within the age
// window. If maxAge is zero, all files pass.
func (w *Walker) passesAge(mtime, now time.Time) bool {
	if w.maxAge == 0 {
		return true
	}
	return now.Sub(mtime) <= w.maxAge
}
