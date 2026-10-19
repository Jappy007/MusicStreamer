package subsonic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/cache"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
)

func testServer(t *testing.T) (*Server, *cache.Manager) {
	t.Helper()
	dir := t.TempDir()
	store, err := cache.New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	art, err := cache.New(dir+"-art", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		user: "admin", pass: "secret", storage: store, artwork: art,
		proxy: http.DefaultClient, coverSem: make(chan struct{}, 1),
	}, store
}

func TestStreamClientIgnoresPort(t *testing.T) {
	r1 := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	r1.RemoteAddr = "1.2.3.4:1111"
	r1.Header.Set("User-Agent", "Psysonic")
	r2 := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	r2.RemoteAddr = "1.2.3.4:2222"
	r2.Header.Set("User-Agent", "Psysonic")
	if streamClient(r1) != streamClient(r2) {
		t.Fatalf("source port must not split a client: %q / %q", streamClient(r1), streamClient(r2))
	}
	s := &Server{}
	s.pin(streamClient(r1), "song-a", variantPreview)
	if got := s.pinnedVariant(streamClient(r2), "song-a"); got != variantPreview {
		t.Fatalf("pinned across ports = %d", got)
	}
	s.pin(streamClient(r2), "song-b", variantFull)
	if got := s.pinnedVariant(streamClient(r1), "song-b"); got != variantFull {
		t.Fatalf("second track pin = %d", got)
	}
}

func TestServePreviewCachesIgnoredRange(t *testing.T) {
	payload := bytes.Repeat([]byte("ABCDEFGH"), 2048)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mp4")
		_, _ = w.Write(payload) // deliberately ignores Range, like YouTube sometimes does
	}))
	defer upstream.Close()

	prev, err := cache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{preview: prev, proxy: http.DefaultClient}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	req.Header.Set("Range", "bytes=100-199")
	s.servePreview(rec, req, "rangesong", ytaudio.Stream{URL: upstream.URL, ContentType: "audio/mp4"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[100:200]) {
		t.Fatalf("range body mismatch: %d bytes", rec.Body.Len())
	}
	if _, ok := prev.Lookup("rangesong"); !ok {
		t.Fatal("ignored-range response should be cached for later seeks")
	}
}

func TestServeCoverCachesImage(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0}, 32)...)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	art, err := cache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{artwork: art, proxy: http.DefaultClient, coverSem: make(chan struct{}, 1)}
	key := metadata.EncodeID("Artist", "Album", "")
	s.coverThumbs = map[string]string{key: upstream.URL}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rest/getCoverArt?id="+key, nil)
	s.serveCover(rec, req, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("cover status = %d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatal("cover bytes mismatch")
	}
	if _, ok := art.Lookup(key); !ok {
		t.Fatal("cover should be cached")
	}

	upstream.Close()
	rec2 := httptest.NewRecorder()
	s.serveCover(rec2, req, key)
	if rec2.Code != http.StatusOK || !bytes.Equal(rec2.Body.Bytes(), png) {
		t.Fatalf("cached cover failed: %d", rec2.Code)
	}
}

func TestServeCoverFailureIsNot200(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	art, err := cache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{artwork: art, proxy: http.DefaultClient, coverSem: make(chan struct{}, 1)}
	key := metadata.EncodeID("Artist", "Album", "")
	s.coverThumbs = map[string]string{key: upstream.URL}
	rec := httptest.NewRecorder()
	s.serveCover(rec, httptest.NewRequest(http.MethodGet, "/rest/getCoverArt", nil), key)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestNativeLoginAndSongs(t *testing.T) {
	s, store := testServer(t)
	frame := make([]byte, 417*39)
	frame[0], frame[1], frame[2], frame[3] = 0xFF, 0xFB, 0x90, 0x00
	for i := 417; i+4 <= len(frame); i += 417 {
		copy(frame[i:i+4], frame[:4])
	}
	if err := store.Store(metadata.EncodeID("Artist", "Album", "Beta"), bytes.NewReader(frame)); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(metadata.EncodeID("Artist", "Album", "Alpha"), bytes.NewReader(frame)); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(nativeLoginRequest{Username: "admin", Password: "secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.NativeLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%q", rec.Code, rec.Body.String())
	}
	var login nativeLoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.Token == "" || login.UserID == "" || login.SubsonicToken == "" {
		t.Fatalf("login response incomplete: %+v", login)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/song?_sort=title&_order=ASC&_start=0&_end=10", nil)
	req.Header.Set("X-ND-Authorization", "Bearer "+login.Token)
	s.NativeSongs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("songs status = %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Total-Count"); got != "2" {
		t.Fatalf("total = %q", got)
	}
	var songs []nativeSong
	if err := json.Unmarshal(rec.Body.Bytes(), &songs); err != nil {
		t.Fatal(err)
	}
	if len(songs) != 2 || songs[0].Title != "Alpha" || songs[1].Title != "Beta" {
		t.Fatalf("songs = %+v", songs)
	}
	if songs[0].Duration <= 0 || songs[0].CoverArtID == "" || songs[0].LibraryID != 0 {
		t.Fatalf("song fields incomplete: %+v", songs[0])
	}

	// album_id filter
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/song?album_id="+metadata.EncodeID("Artist", "Album", ""), nil)
	req.Header.Set("X-ND-Authorization", "Bearer "+login.Token)
	s.NativeSongs(rec, req)
	if got := rec.Header().Get("X-Total-Count"); got != "2" {
		t.Fatalf("album filter total = %q", got)
	}
}

func TestNativeRoleFiltersReturnEmpty(t *testing.T) {
	s, store := testServer(t)
	if err := store.Store(metadata.EncodeID("Artist", "Album", "Track"), bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/artist?role=albumartist", nil)
	rec := httptest.NewRecorder()
	s.nativeTokens = map[string]nativeSession{"tok": {user: "admin", exp: timeNowPlusHour()}}
	req.Header.Set("X-ND-Authorization", "Bearer tok")
	s.NativeArtists(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var artists []nativeArtist
	_ = json.Unmarshal(rec.Body.Bytes(), &artists)
	if len(artists) != 0 {
		t.Fatalf("role filter should be empty, got %+v", artists)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/album?role_albumartist_id=x", nil)
	req.Header.Set("X-ND-Authorization", "Bearer tok")
	s.NativeAlbums(rec, req)
	var albums []nativeAlbum
	_ = json.Unmarshal(rec.Body.Bytes(), &albums)
	if len(albums) != 0 {
		t.Fatalf("role album filter should be empty, got %+v", albums)
	}
}

func TestNativeUnauthorized(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.NativeSongs(rec, httptest.NewRequest(http.MethodGet, "/api/song", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("expected JSON error, got %q", rec.Body.String())
	}
}

func TestNativeRoutes(t *testing.T) {
	s, _ := testServer(t)
	mux := http.NewServeMux()
	Route(mux, s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("native login route not registered")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/song", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("native song route status = %d", rec.Code)
	}
}

func timeNowPlusHour() time.Time {
	return time.Now().Add(time.Hour)
}
