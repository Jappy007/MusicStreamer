package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// safeID ensures cache keys cannot escape the cache directory.
// Song IDs issued by pkg/metadata are base64.RawURLEncoding, which
// already match this alphabet. Anything else is rejected.
var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// Manager is a filesystem-backed LRU cache for audio bytes.
//
// It keeps a lightweight in-memory index (name -> size/atime) guarded by
// sync.RWMutex so lookups are lock-cheap and never read the disk.
// Eviction deletes least-recently-used files until total <= maxBytes.
//
// Memory footprint is O(number of files), a few dozen bytes per entry.
type Manager struct {
	dir      string
	maxBytes int64

	mu    sync.RWMutex
	size  map[string]int64     // filename -> bytes
	atime map[string]time.Time // filename -> last access
	total int64
}

// New creates (and scans) a cache rooted at dir, capped at maxBytes.
// maxBytes <= 0 means "use 10 GiB default".
func New(dir string, maxBytes int64) (*Manager, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 30
	}
	if dir == "" {
		return nil, errors.New("cache: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		dir:      dir,
		maxBytes: maxBytes,
		size:     make(map[string]int64),
		atime:    make(map[string]time.Time),
	}
	if err := m.scan(); err != nil {
		return nil, err
	}
	return m, nil
}

// scan rebuilds the in-memory index from files on disk.
// Uses mtime as a stand-in for atime (portable; Atime is not reliable
// across filesystems / Docker volumes).
func (m *Manager) scan() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip temp / sidecar files.
		if filepath.Ext(name) == ".tmp" || filepath.Ext(name) == ".meta" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		m.size[name] = info.Size()
		m.atime[name] = info.ModTime()
		m.total += info.Size()
	}
	return nil
}

// validate confines names to the cache alphabet.
func validate(name string) error {
	if !safeIDRe.MatchString(name) {
		return errors.New("cache: unsafe key")
	}
	return nil
}

func (m *Manager) absPath(name string) string {
	return filepath.Join(m.dir, name)
}

// Lookup returns the absolute path of a cached entry, if present.
// It updates recency (both in-memory and via os.Chtimes for persistence).
func (m *Manager) Lookup(name string) (string, bool) {
	if validate(name) != nil {
		return "", false
	}
	m.mu.RLock()
	_, ok := m.size[name]
	m.mu.RUnlock()
	if !ok {
		return "", false
	}
	p := m.absPath(name)
	if _, err := os.Stat(p); err != nil {
		// File vanished out from under us; drop index entry.
		m.mu.Lock()
		if s, ok := m.size[name]; ok {
			m.total -= s
			delete(m.size, name)
			delete(m.atime, name)
		}
		m.mu.Unlock()
		return "", false
	}
	m.Touch(name)
	return p, true
}

// Touch bumps recency for name.
func (m *Manager) Touch(name string) {
	now := time.Now()
	m.mu.Lock()
	if _, ok := m.size[name]; ok {
		m.atime[name] = now
	}
	m.mu.Unlock()
	// Best-effort mtime bump so recency survives restarts. Ignore errors
	// (read-only mounts, etc. must not break streaming).
	_ = os.Chtimes(m.absPath(name), now, now)
}

// Store atomically writes r to name via tmp-file + rename, then enforces
// the size ceiling. It streams with a bounded buffer; it never loads the
// whole body into RAM.
func (m *Manager) Store(name string, r io.Reader) error {
	if err := validate(name); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	size, werr := io.Copy(tmp, r)
	cerr := tmp.Close()
	if werr != nil {
		_ = os.Remove(tmpName)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(tmpName)
		return cerr
	}
	// Ensure bytes hit disk before rename (durability on unclean shutdown).
	if f, err := os.Open(tmpName); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	dst := m.absPath(name)
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	now := time.Now()
	_ = os.Chtimes(dst, now, now)

	m.mu.Lock()
	if old, ok := m.size[name]; ok {
		m.total -= old
	}
	m.size[name] = size
	m.atime[name] = now
	m.total += size
	over := m.total > m.maxBytes
	m.mu.Unlock()

	if over {
		m.Enforce()
	}
	return nil
}

// Enforce deletes least-recently-used entries until total <= maxBytes.
// Called after every Store and periodically by the background cleaner.
func (m *Manager) Enforce() {
	m.mu.Lock()
	if m.total <= m.maxBytes {
		m.mu.Unlock()
		return
	}
	type kv struct {
		name  string
		atime time.Time
	}
	ordered := make([]kv, 0, len(m.size))
	for n, t := range m.atime {
		ordered = append(ordered, kv{n, t})
	}
	// Oldest first.
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].atime.Before(ordered[j].atime)
	})
	var victims []string
	var freed int64
	for _, v := range ordered {
		if m.total-freed <= m.maxBytes {
			break
		}
		victims = append(victims, v.name)
		freed += m.size[v.name]
	}
	for _, v := range victims {
		delete(m.size, v)
		delete(m.atime, v)
	}
	m.total -= freed
	m.mu.Unlock()

	// Delete outside the lock so slow disks don't stall lookups.
	for _, v := range victims {
		_ = os.Remove(m.absPath(v))
	}
}

// Stats returns (entries, totalBytes, maxBytes) for logging/health.
func (m *Manager) Stats() (int, int64, int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.size), m.total, m.maxBytes
}

// IDs returns a snapshot of the cache keys (song IDs), sorted. Because keys
// are the reversible Subsonic song IDs, callers can rebuild track identity
// (artist/album/title) from them — this is how the cache is surfaced as a
// browsable library.
func (m *Manager) IDs() []string {
	m.mu.RLock()
	ids := make([]string, 0, len(m.size))
	for n := range m.size {
		ids = append(ids, n)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Dir exposes the cache root for staging temp downloads.
func (m *Manager) Dir() string { return m.dir }

// CommitTemp atomically promotes an already-written temp file (created in
// Dir, e.g. via os.CreateTemp) to a cache entry and enforces the ceiling.
// On any error the temp file is removed.
func (m *Manager) CommitTemp(name string, tmpPath string) error {
	if err := validate(name); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	dst := m.absPath(name)
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	now := time.Now()
	_ = os.Chtimes(dst, now, now)
	m.mu.Lock()
	if old, ok := m.size[name]; ok {
		m.total -= old
	}
	m.size[name] = info.Size()
	m.atime[name] = now
	m.total += info.Size()
	over := m.total > m.maxBytes
	m.mu.Unlock()
	if over {
		m.Enforce()
	}
	return nil
}

// StartCleaner runs Enforce on every tick until ctx is done.
// It is a no-op memory-wise: one goroutine + one timer.
func StartCleaner(m *Manager, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.Enforce()
		}
	}
}
