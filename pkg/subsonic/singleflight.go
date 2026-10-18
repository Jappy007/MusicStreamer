package subsonic

import (
	"sync"
	"time"
)

// flight coalesces concurrent work by key: the first caller for a key becomes
// the leader and runs the work; later callers with the same key are told to
// stand down (the work is already in progress). Used to ensure a track is
// background-fetched exactly once even if many stream requests race for it.
type flight struct {
	mu sync.Mutex
	m  map[string]struct{}
}

// begin records key as in-flight. It returns leader=true if the caller now
// owns the work and must call finish(key) when done; leader=false means another
// goroutine already holds it.
func (f *flight) begin(key string) (leader bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[key]; ok {
		return false
	}
	if f.m == nil {
		f.m = make(map[string]struct{})
	}
	f.m[key] = struct{}{}
	return true
}

// finish clears key, allowing a future fetch (e.g. after LRU eviction).
func (f *flight) finish(key string) {
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
}

// --- per-connection stream variant pinning --------------------------------

// Stream variants a client connection may be pinned to for the current track.
const (
	variantNone = iota
	variantFull
	variantPreview
)

// pinTTL bounds how long a connection's variant is remembered, so the map
// cannot grow without bound across long-lived keep-alive connections.
const pinTTL = 30 * time.Minute

type pinEntry struct {
	id      string
	variant int
	at      time.Time
}

// pin records which file variant a connection is playing for id, so later
// Range (seek) requests for the same track stay on the same file and keep
// their byte offsets valid.
func (s *Server) pin(remoteAddr, id string, variant int) {
	s.pinMu.Lock()
	if s.pins == nil {
		s.pins = make(map[string]pinEntry)
	}
	now := time.Now()
	for k, e := range s.pins {
		if now.Sub(e.at) > pinTTL {
			delete(s.pins, k)
		}
	}
	s.pins[remoteAddr] = pinEntry{id: id, variant: variant, at: now}
	s.pinMu.Unlock()
}

// pinnedVariant returns the variant a connection is pinned to for id, or
// variantNone if unknown.
func (s *Server) pinnedVariant(remoteAddr, id string) int {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	e, ok := s.pins[remoteAddr]
	if !ok || e.id != id {
		return variantNone
	}
	return e.variant
}
