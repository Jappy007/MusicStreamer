package subsonic

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debridsubsonic/debridsubsonic/pkg/audioinfo"
	"github.com/debridsubsonic/debridsubsonic/pkg/cache"
	"github.com/debridsubsonic/debridsubsonic/pkg/metadata"
	"github.com/debridsubsonic/debridsubsonic/pkg/soulseek"
)

// Server wires config + collaborators to Subsonic HTTP handlers.
type Server struct {
	user     string
	pass     string
	meta     *metadata.Client
	soulseek *soulseek.Client
	storage  *cache.Manager

	// proxy performs upstream GETs (cover art). No client-level Timeout:
	// the request context governs each fetch.
	proxy *http.Client

	mu       sync.Mutex
	inflight map[string]struct{} // song IDs currently being resolved/fetched

	// info memoizes probed audio properties (format/sample rate/bit depth)
	// per cached song id; size invalidates the entry on re-download.
	infoMu sync.Mutex
	info   map[string]infoCache
}

// infoCache stores a probed track's properties alongside the file size they
// were derived from.
type infoCache struct {
	size int64
	info audioinfo.Info
}

// NewServer builds a handler set. proxy may be nil (a default is used).
// sk may be nil (disables Soulseek fetching).
func NewServer(user, pass string, m *metadata.Client, sk *soulseek.Client, s *cache.Manager, proxy *http.Client) *Server {
	if proxy == nil {
		proxy = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        128,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &Server{
		user: user, pass: pass,
		meta: m, soulseek: sk, storage: s,
		proxy: proxy, inflight: make(map[string]struct{}),
	}
}

// authed writes a 40-error and returns false when credentials fail.
func (s *Server) authed(w http.ResponseWriter, r *http.Request) bool {
	if Authenticate(r, s.user, s.pass) {
		return true
	}
	log.Printf("auth failed: %s u=%q from %s", r.URL.Path, r.FormValue("u"), r.RemoteAddr)
	writeFail(w, r, 40, "Wrong username or password")
	return false
}

// --- Simple endpoints -----------------------------------------------------

func (s *Server) Ping(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	write(w, r, okResponse())
}

func (s *Server) License(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.License = &License{Valid: true, Email: "debrid@local", Key: "debridsubsonic", Date: "2030-01-01"}
	write(w, r, resp)
}

func (s *Server) MusicFolders(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.MusicFolders = &MusicFolders{Folders: []Folder{{ID: 0, Name: "Debrid"}}}
	write(w, r, resp)
}

// GetUser answers Feishin's login probe. The single configured user gets
// full playback/playlist rights; upload/admin-only flags stay off.
func (s *Server) GetUser(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.User = &User{
		Username:          s.user,
		ScrobblingEnabled: false,
		AdminRole:         true,
		SettingsRole:      true,
		DownloadRole:      true,
		UploadRole:        false,
		PlaylistRole:      true,
		CoverArtRole:      true,
		CommentRole:       false,
		PodcastRole:       false,
		StreamRole:        true,
		JukeboxRole:       false,
		ShareRole:         false,
		Folders:           []int{0},
	}
	write(w, r, resp)
}

// Empty-library stubs: playlists/genres/starred have no persistent store on
// this server (tracks resolve via metadata search + debrid). Album/artist
// browse is cache-backed; see catalog.go.
func (s *Server) Playlists(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Playlists = &Playlists{}
	write(w, r, resp)
}

func (s *Server) Genres(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Genres = &Genres{}
	write(w, r, resp)
}

func (s *Server) AlbumList(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.AlbumList = &AlbumList{Albums: s.catalog().albumEntries()}
	write(w, r, resp)
}

func (s *Server) Starred(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Starred = &Starred{}
	write(w, r, resp)
}

func (s *Server) Starred2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Starred2 = &Starred2{}
	write(w, r, resp)
}

func (s *Server) AlbumList2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.AlbumList2 = &AlbumList2{Albums: s.catalog().albumEntries()}
	write(w, r, resp)
}

// GetAlbum resolves an album ID (artist+album, empty title segment) into a
// playable track list via metadata search. This is what Feishin renders as
// the album page; each song streams through the Tier 1-3 pipeline.
func (s *Server) GetAlbum(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	artist, album, title, err := metadata.SplitID(id)
	if err != nil {
		writeFail(w, r, 70, "Album not found")
		return
	}
	if album == "" {
		album = title // tolerate song IDs passed as album refs
	}
	if strings.TrimSpace(artist+album) == "" {
		writeFail(w, r, 70, "Album not found")
		return
	}
	// Cached tracks are real, already-downloaded files: prefer them so the
	// album page lists exactly what can play instantly.
	if songs := s.catalog().albumSongs(artist, album); len(songs) > 0 {
		resp := okResponse()
		info := &AlbumInfo{
			ID: id, Name: displayAlbum(album), Artist: artist,
			ArtistID: metadata.EncodeID(artist, "", ""),
			CoverArt: id,
		}
		info.Songs = songs
		info.SongCount = len(songs)
		resp.Album = info
		write(w, r, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	songs, err := s.meta.Search(ctx, strings.TrimSpace(artist+" "+album), 30)
	if err != nil || len(songs) == 0 {
		writeFail(w, r, 70, "Album not found")
		return
	}
	// Prefer tracks from the exact album; fall back to all results.
	var tracks []metadata.Song
	for _, m := range songs {
		if strings.EqualFold(m.Album, album) {
			tracks = append(tracks, m)
		}
	}
	if len(tracks) == 0 {
		tracks = songs
	}
	resp := okResponse()
	info := &AlbumInfo{
		ID: id, Name: album, Artist: artist,
		ArtistID: metadata.EncodeID(artist, "", ""),
		CoverArt: id, // resolves back through getCoverArt
	}
	for _, m := range tracks {
		info.Songs = append(info.Songs, songToEntry(m))
		info.Duration += m.Duration
	}
	info.SongCount = len(info.Songs)
	resp.Album = info
	write(w, r, resp)
}

// CoverArt proxies the Deezer/MusicBrainz artwork for a song/album/artist
// ID. Cover IDs are our own song/album IDs, so decode -> metadata search ->
// stream the upstream image bytes (capped, timed out). No caching: CDN hits
// are cheap and this keeps the audio LRU accounting pure.
func (s *Server) CoverArt(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	artist, album, title, err := metadata.SplitID(id)
	if err != nil {
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	query := strings.TrimSpace(strings.TrimSpace(artist+" "+album) + " " + title)
	if query == "" {
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	songs, err := s.meta.Search(ctx, query, 5)
	if err != nil {
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	coverURL := ""
	for _, m := range songs {
		if m.CoverURL != "" {
			coverURL = m.CoverURL
			break
		}
	}
	if coverURL == "" {
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	ictx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ictx, http.MethodGet, coverURL, nil)
	if err != nil {
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	upResp, err := s.proxy.Do(req)
	if err != nil || upResp.StatusCode != http.StatusOK {
		if upResp != nil {
			_ = upResp.Body.Close()
		}
		writeFail(w, r, 70, "Cover art not found")
		return
	}
	defer upResp.Body.Close()
	ctype := upResp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "image/jpeg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.CopyBuffer(w, io.LimitReader(upResp.Body, 8<<20), make([]byte, 32<<10))
}

// Jukebox answers jukeboxControl probes with an empty stopped queue.
// There is no server-side player; control actions are accepted and ignored.
func (s *Server) Jukebox(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Jukebox = &JukeboxStatus{CurrentIndex: -1, Playing: false}
	write(w, r, resp)
}

// GetArtist resolves an artist ID into their album list via metadata
// search. Albums link back into getAlbum, so the whole artist page is
// browsable and playable.
func (s *Server) GetArtist(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	artist, album, title, err := metadata.SplitID(id)
	if err != nil {
		writeFail(w, r, 70, "Artist not found")
		return
	}
	if artist == "" {
		artist = album
		if artist == "" {
			artist = title
		}
	}
	if strings.TrimSpace(artist) == "" {
		writeFail(w, r, 70, "Artist not found")
		return
	}
	// Prefer cached (already-downloaded) albums so the artist page reflects
	// what is immediately playable.
	if albums := s.catalog().artistAlbums(artist); len(albums) > 0 {
		resp := okResponse()
		info := &ArtistInfo{ID: id, Name: artist, CoverArt: id, Albums: albums, AlbumCount: len(albums)}
		resp.Artist = info
		write(w, r, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	songs, err := s.meta.Search(ctx, artist, 50)
	if err != nil || len(songs) == 0 {
		writeFail(w, r, 70, "Artist not found")
		return
	}
	// Group tracks into distinct albums.
	byAlbum := make(map[string]*AlbumEntry)
	var order []string
	for _, m := range songs {
		if m.Album == "" {
			continue
		}
		e, ok := byAlbum[m.Album]
		if !ok {
			aid := metadata.EncodeID(m.Artist, m.Album, "")
			e = &AlbumEntry{ID: aid, Name: m.Album, Artist: m.Artist,
				ArtistID: metadata.EncodeID(m.Artist, "", ""), CoverArt: aid}
			byAlbum[m.Album] = e
			order = append(order, m.Album)
		}
		e.SongCount++
		e.Duration += m.Duration
	}
	resp := okResponse()
	info := &ArtistInfo{ID: id, Name: artist, CoverArt: id}
	for _, name := range order {
		info.Albums = append(info.Albums, *byAlbum[name])
	}
	info.AlbumCount = len(info.Albums)
	resp.Artist = info
	write(w, r, resp)
}

// Scrobble accepts play-event reports and discards them (no listening
// history is stored). Always ok so clients don't retry.
func (s *Server) Scrobble(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	write(w, r, okResponse())
}

func (s *Server) Indexes(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Artists = &Artists{Ignored: "The An A", List: s.catalog().artistEntries()}
	write(w, r, resp)
}

func (s *Server) OpenExtensions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Extensions = &Extensions{List: []Extension{{Name: "search3", Versions: []int{1}}}}
	resp.ServerInfo = &ServerInfo{Version: serverVersion}
	write(w, r, resp)
}

// --- search3 --------------------------------------------------------------

func songToEntry(m metadata.Song) SongEntry {
	suffix, ctype := "mp3", "audio/mpeg"
	if m.CoverURL != "" {
		// Deezer-sourced; actual bytes resolve via debrid and are usually FLAC.
		suffix, ctype = "flac", "audio/flac"
	}
	return SongEntry{
		ID: m.ID, Parent: m.AlbumID, IsDir: false,
		Title: m.Title, Album: m.Album, Artist: m.Artist,
		Track: m.Track, CoverArt: m.ID, Duration: m.Duration,
		ContentType: ctype, Suffix: suffix,
		BitRate: 1411,
		Path:    fmt.Sprintf("%s/%s/%s.%s", m.Artist, m.Album, m.Title, suffix),
		AlbumID: m.AlbumID, ArtistID: m.ArtistID, Type: "music",
	}
}

func (s *Server) Search3(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	query := strings.TrimSpace(firstOf(r.Form, "query", "q"))
	songCount := atoiWithDefault(firstOf(r.Form, "songCount", "count"), 20)
	if songCount < 1 {
		songCount = 20
	}
	if songCount > 500 {
		songCount = 500
	}
	offset := atoiWithDefault(firstOf(r.Form, "songOffset", "offset"), 0)
	if offset < 0 {
		offset = 0
	}

	// A flat list of every cached track. This is what clients (Feishin's
	// Songs view, and anything else that paginates the whole library) hit:
	// they call search3 with a query of "" and page via songOffset/songCount.
	// Each entry is one cached file — a track, exposed on a track-by-track
	// basis, not rolled up into albums or artists.
	cat := s.catalog()
	var entries []SongEntry
	if query == "" {
		entries = make([]SongEntry, 0, len(cat.songs))
		for _, cs := range cat.songs {
			entries = append(entries, cat.entry(cs))
		}
	} else {
		seen := make(map[string]bool)
		for _, e := range cat.searchSongs(query) { // cached matches first
			if !seen[e.ID] {
				seen[e.ID] = true
				entries = append(entries, e)
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		songs, err := s.meta.Search(ctx, query, songCount)
		if err != nil && len(entries) == 0 {
			writeFail(w, r, 0, "search failed: "+err.Error())
			return
		}
		for _, m := range songs {
			e := songToEntry(m)
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			entries = append(entries, e)
		}
	}

	if offset > len(entries) {
		offset = len(entries)
	}
	end := offset + songCount
	if end > len(entries) {
		end = len(entries)
	}
	resp := okResponse()
	resp.SearchResult3 = &SearchResult{Songs: entries[offset:end]}
	write(w, r, resp)
}

// RandomSongs returns every cached track as a flat list (the standard
// "all songs" endpoint). Ordering is stable so clients can page through it.
func (s *Server) RandomSongs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	size := atoiWithDefault(firstOf(r.Form, "size", "count"), 500)
	if size < 1 {
		size = 500
	}
	if size > 5000 {
		size = 5000
	}
	cat := s.catalog()
	n := len(cat.songs)
	if size < n {
		n = size
	}
	songs := make([]SongEntry, 0, n)
	for _, cs := range cat.songs[:n] {
		songs = append(songs, cat.entry(cs))
	}
	resp := okResponse()
	resp.RandomSongs = &RandomSongs{Songs: songs}
	write(w, r, resp)
}

// --- getSong --------------------------------------------------------------

func (s *Server) GetSong(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	artist, album, title, err := metadata.DecodeID(id)
	if err != nil {
		writeFail(w, r, 70, "Song not found")
		return
	}
	// Cached track: return it as-is (no network round-trip needed).
	if e, ok := s.catalog().songForID(id); ok {
		resp := okResponse()
		resp.Song = &e
		write(w, r, resp)
		return
	}
	m := metadata.Song{
		ID: id, Artist: artist, Album: album, Title: title,
		AlbumID:  metadata.EncodeID(artist, album, ""),
		ArtistID: metadata.EncodeID(artist, "", ""),
	}
	// Best-effort enrichment (duration/cover) without failing the request.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if found, serr := s.meta.Search(ctx, artist+" "+title, 3); serr == nil {
		for _, f := range found {
			if f.Artist == artist || strings.EqualFold(f.Title, title) {
				m.Duration = f.Duration
				m.CoverURL = f.CoverURL
				m.Track = f.Track
				break
			}
		}
	}
	resp := okResponse()
	e := songToEntry(m)
	resp.Song = &e
	write(w, r, resp)
}

// --- stream: cache -> Soulseek pipeline -----------------------------------

// Stream implements GET /rest/stream.view?id=... Two sources, tried in
// order until one serves audio:
//
// Tier 1: local disk cache (instant, Range-capable).
// Tier 3: Soulseek direct via slskd (music-native, often fastest).
func (s *Server) Stream(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	if id == "" {
		writeFail(w, r, 10, "Missing id")
		return
	}
	artist, album, title, err := metadata.DecodeID(id)
	if err != nil {
		writeFail(w, r, 70, "Song not found")
		return
	}

	// Tier 1: local disk cache. ServeFile/ServeContent handles
	// `Range: bytes=..` -> 206 Partial Content for seeking.
	if path, ok := s.storage.Lookup(id); ok {
		log.Printf("stream: %q - %q: tier=cache", artist, title)
		s.serveCached(w, r, path)
		return
	}

	// Dedupe thundering herds: one resolution per song at a time.
	if !s.claim(id) {
		// Another goroutine is fetching (a slow Soulseek download). Wait
		// for the cache to fill — the fetch writes it there on success —
		// bounded by the client's own request context.
		if s.waitForCache(r.Context(), id, 10*time.Minute) {
			if path, ok := s.storage.Lookup(id); ok {
				s.serveCached(w, r, path)
				return
			}
		}
		writeFail(w, r, 0, "Track is being fetched, retry shortly")
		return
	}
	defer s.release(id)

	// Tier 3: Soulseek direct (music-native network, often fastest for
	// audio). Needs slskd configured (SLSKD_URL/SLSKD_DOWNLOAD_DIR).
	if s.soulseek != nil && s.soulseek.Enabled() {
		if path, err := s.soulseek.FetchTrackShared(r.Context(), artist, album, title); err != nil {
			log.Printf("stream: %q - %q: soulseek failed: %v", artist, title, err)
		} else {
			log.Printf("stream: %q - %q: tier=soulseek", artist, title)
			s.serveSoulseek(w, r, id, path)
			return
		}
	}
	log.Printf("stream: %q - %q: FAILED (no source resolved)", artist, title)
	writeFail(w, r, 70, "No source for \""+artist+" - "+title+"\"")
}

// and promotes it into the disk cache in the background. Serving first means
// playback starts immediately; the background copy (atomic tmp+rename) makes
// later plays, concurrent clients and Range seeks converge on a stable cache
// entry even if slskd later prunes the download.
func (s *Server) serveSoulseek(w http.ResponseWriter, r *http.Request, id, path string) {
	if f, err := os.Open(path); err == nil {
		go func() {
			defer f.Close()
			if serr := s.storage.Store(id, f); serr != nil {
				log.Printf("stream: cache store %s: %v", id, serr)
			}
		}()
	} else {
		log.Printf("stream: open soulseek file %s: %v", path, err)
	}
	s.serveCached(w, r, path)
}

// serveCached serves a cache file with sniffed content type and full
// http.ServeContent Range/206 support.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeFail(w, r, 70, "Cached file unavailable")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeFail(w, r, 70, "Cached file unavailable")
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := http.DetectContentType(head[:n])
	if strings.HasPrefix(ctype, "text/") || ctype == "application/octet-stream" {
		ctype = "audio/mpeg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	_, _ = f.Seek(0, io.SeekStart)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// claim/inflight helpers -----------------------------------------------

func (s *Server) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inflight[id]; ok {
		return false
	}
	s.inflight[id] = struct{}{}
	return true
}

func (s *Server) release(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

func (s *Server) waitForCache(ctx context.Context, id string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
		if _, ok := s.storage.Lookup(id); ok {
			return true
		}
		s.mu.Lock()
		_, busy := s.inflight[id]
		s.mu.Unlock()
		if !busy {
			return false
		}
	}
	return false
}

// --- helpers ------------------------------------------------------------

func firstOf(form map[string][]string, keys ...string) string {
	for _, k := range keys {
		if v := form[k]; len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}

func atoiWithDefault(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
