package subsonic

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/cache"
	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
)

func req(rangeHeader string) *http.Request {
	r := &http.Request{Header: http.Header{}}
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	return r
}

func TestRangeIsFresh(t *testing.T) {
	cases := []struct {
		header string
		fresh  bool
	}{
		{"", true},
		{"bytes=0-", true},
		{"bytes=0-1023", true},
		{"bytes=1024-", false},
		{"bytes=500-1000", false},
		{"bytes=-500", false},          // suffix range
		{"bytes=0-100, 200-300", true}, // first range starts at 0
		{"bytes=5-100, 0-4", false},    // first range starts at 5
		{"items=0-1", true},            // non-bytes unit -> treat as fresh
		{"bytes=", true},
	}
	for _, c := range cases {
		if got := rangeIsFresh(req(c.header)); got != c.fresh {
			t.Errorf("rangeIsFresh(%q) = %v, want %v", c.header, got, c.fresh)
		}
	}
}

func TestFlightCoalesces(t *testing.T) {
	var f flight
	if !f.begin("a") {
		t.Fatal("first begin should lead")
	}
	if f.begin("a") {
		t.Fatal("second begin for same key should not lead")
	}
	if !f.begin("b") {
		t.Fatal("different key should lead")
	}
	f.finish("a")
	if !f.begin("a") {
		t.Fatal("after finish, key may lead again")
	}
}

func TestFlightSingleExecution(t *testing.T) {
	var f flight
	var runs int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if f.begin("k") {
				atomic.AddInt32(&runs, 1)
				time.Sleep(5 * time.Millisecond)
				f.finish("k")
			}
		}()
	}
	close(start)
	wg.Wait()
	if runs != 1 {
		t.Fatalf("expected exactly one run, got %d", runs)
	}
}

func TestPinning(t *testing.T) {
	s := &Server{}
	s.pin("1.2.3.4:1111", "song1", variantPreview)
	if got := s.pinnedVariant("1.2.3.4:1111", "song1"); got != variantPreview {
		t.Fatalf("pinned = %d, want preview", got)
	}
	if got := s.pinnedVariant("1.2.3.4:1111", "song2"); got != variantNone {
		t.Fatalf("different id should be unpinned, got %d", got)
	}
	if got := s.pinnedVariant("9.9.9.9:2222", "song1"); got != variantNone {
		t.Fatalf("different conn should be unpinned, got %d", got)
	}
	// A fresh start re-pins to the full variant (the upgrade path).
	s.pin("1.2.3.4:1111", "song1", variantFull)
	if got := s.pinnedVariant("1.2.3.4:1111", "song1"); got != variantFull {
		t.Fatalf("re-pin = %d, want full", got)
	}
}

func TestServePreviewFullAndRange(t *testing.T) {
	payload := bytes.Repeat([]byte("ABCDEFGH"), 2048) // 16 KiB
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mp4")
		http.ServeContent(w, r, "a.m4a", time.Time{}, bytes.NewReader(payload))
	}))
	defer upstream.Close()

	prev, err := cache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{preview: prev, proxy: http.DefaultClient}

	// Full GET: streamed to the client AND committed to the preview cache.
	id := "testsongidfull"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	s.servePreview(rec, req, id, ytaudio.Stream{URL: upstream.URL, ContentType: "audio/mp4"})
	if rec.Code != http.StatusOK {
		t.Fatalf("full status = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("full body mismatch: %d bytes", rec.Body.Len())
	}
	path, ok := prev.Lookup(id)
	if !ok {
		t.Fatal("full GET should populate the preview cache")
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != int64(len(payload)) {
		t.Fatalf("cached size wrong: %v", err)
	}

	// Range GET: proxied as 206 and never cached.
	id2 := "testsongidrange"
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	req2.Header.Set("Range", "bytes=100-199")
	s.servePreview(rec2, req2, id2, ytaudio.Stream{URL: upstream.URL, ContentType: "audio/mp4"})
	if rec2.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, body=%q", rec2.Code, rec2.Body.String())
	}
	if !bytes.Equal(rec2.Body.Bytes(), payload[100:200]) {
		t.Fatalf("range body mismatch: %d bytes", rec2.Body.Len())
	}
	if _, ok := prev.Lookup(id2); ok {
		t.Fatal("range response must not be cached")
	}
}

func TestPreviewURLMemo(t *testing.T) {
	s := &Server{}
	st := ytaudio.Stream{URL: "https://example/x", ContentType: "audio/mp4"}

	if _, ok := s.cachedPreviewURL("a"); ok {
		t.Fatal("unexpected initial hit")
	}
	s.rememberPreviewURL("a", st)
	got, ok := s.cachedPreviewURL("a")
	if !ok || got.URL != st.URL || got.ContentType != st.ContentType {
		t.Fatalf("memo miss: %+v ok=%v", got, ok)
	}

	// awaitPreviewURL polls until another request publishes the result.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.rememberPreviewURL("b", st)
	}()
	if got, ok := s.awaitPreviewURL(context.Background(), "b", time.Second); !ok || got.URL != st.URL {
		t.Fatalf("await failed: %+v ok=%v", got, ok)
	}

	// It gives up when the caller's context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := s.awaitPreviewURL(ctx, "missing", 5*time.Second); ok {
		t.Fatal("await should stop on cancelled context")
	}
}
