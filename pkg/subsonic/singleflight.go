package subsonic

import (
	"context"
	"sync"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
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

// --- short-lived resolved-preview-URL memo --------------------------------

// previewURLTTL bounds how long a resolved YouTube URL is reused. The upstream
// URL itself expires after several hours; 10 minutes is deliberately short and
// only needs to cover a client's probe-then-retry or a quick replay.
const previewURLTTL = 10 * time.Minute

type previewURL struct {
	st  ytaudio.Stream
	exp time.Time
}

// rememberPreviewURL stores a freshly resolved URL for id and purges expired
// entries while it is there.
func (s *Server) rememberPreviewURL(id string, st ytaudio.Stream) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := time.Now()
	if s.previewURLs == nil {
		s.previewURLs = make(map[string]previewURL)
	}
	for k, e := range s.previewURLs {
		if now.After(e.exp) {
			delete(s.previewURLs, k)
		}
	}
	s.previewURLs[id] = previewURL{st: st, exp: now.Add(previewURLTTL)}
	delete(s.previewFail, id)
}

// previewFailTTL is how long a failed lookup suppresses retries, so repeated
// list refreshes do not re-run yt-dlp for a track that just failed.
const previewFailTTL = 10 * time.Minute

// markPreviewFailed records that resolving id failed, to back off briefly.
func (s *Server) markPreviewFailed(id string) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := time.Now()
	if s.previewFail == nil {
		s.previewFail = make(map[string]time.Time)
	}
	for k, t := range s.previewFail {
		if now.Sub(t) > previewFailTTL {
			delete(s.previewFail, k)
		}
	}
	s.previewFail[id] = now
}

// previewFailed reports whether id failed to resolve recently.
func (s *Server) previewFailed(id string) bool {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	t, ok := s.previewFail[id]
	if !ok {
		return false
	}
	if time.Since(t) > previewFailTTL {
		delete(s.previewFail, id)
		return false
	}
	return true
}

// cachedPreviewURL returns a still-valid resolved URL for id.
func (s *Server) cachedPreviewURL(id string) (ytaudio.Stream, bool) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	e, ok := s.previewURLs[id]
	if !ok || time.Now().After(e.exp) {
		return ytaudio.Stream{}, false
	}
	return e.st, true
}

// awaitPreviewURL waits up to max for another request's in-flight resolve of id
// to publish its URL, so racing/probing requests do not each spawn yt-dlp.
func (s *Server) awaitPreviewURL(ctx context.Context, id string, max time.Duration) (ytaudio.Stream, bool) {
	deadline := time.Now().Add(max)
	for {
		if st, ok := s.cachedPreviewURL(id); ok {
			return st, true
		}
		if time.Now().After(deadline) {
			return ytaudio.Stream{}, false
		}
		select {
		case <-ctx.Done():
			return ytaudio.Stream{}, false
		case <-time.After(100 * time.Millisecond):
		}
	}
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
