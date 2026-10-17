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

	"github.com/debridsubsonic/debridsubsonic/pkg/cache"
	"github.com/debridsubsonic/debridsubsonic/pkg/debrid"
	"github.com/debridsubsonic/debridsubsonic/pkg/library"
	"github.com/debridsubsonic/debridsubsonic/pkg/metadata"
	"github.com/debridsubsonic/debridsubsonic/pkg/soulseek"
)

// Server wires config + collaborators to Subsonic HTTP handlers.
type Server struct {
	user     string
	pass     string
	meta     *metadata.Client
	debrid   *debrid.Client
	indexer  *debrid.Prowlarr
	public   *debrid.PublicIndex
	soulseek *soulseek.Client
	library  *library.Library
	storage  *cache.Manager

	// proxy performs upstream GETs to AllDebrid CDN URLs. No client-level
	// Timeout: the request context (client connection) governs the stream.
	proxy *http.Client

	mu       sync.Mutex
	inflight map[string]struct{} // song IDs currently being resolved/fetched
}

// NewServer builds a handler set. proxy may be nil (a default is used).
// public may be nil (disables the zero-config public-index fallback).
// sk may be nil (disables Soulseek fetching).
// lib may be nil (disables the local-library tier).
func NewServer(user, pass string, m *metadata.Client, d *debrid.Client, p *debrid.Prowlarr, pub *debrid.PublicIndex, sk *soulseek.Client, lib *library.Library, s *cache.Manager, proxy *http.Client) *Server {
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
		meta: m, debrid: d, indexer: p, public: pub, soulseek: sk, library: lib, storage: s,
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

// Library stubs: this server has no persistent library (tracks resolve via
// metadata search + debrid), so these return valid empty envelopes.
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
	resp.AlbumList = &AlbumList{}
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
	resp.AlbumList2 = &AlbumList2{}
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
	resp.Artists = &Artists{Ignored: "The An A"}
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
	if songCount > 50 {
		songCount = 50
	}
	if query == "" {
		resp := okResponse()
		resp.SearchResult3 = &SearchResult{}
		write(w, r, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	songs, err := s.meta.Search(ctx, query, songCount)
	if err != nil {
		writeFail(w, r, 0, "search failed: "+err.Error())
		return
	}
	resp := okResponse()
	sr := &SearchResult{}
	for _, m := range songs {
		sr.Songs = append(sr.Songs, songToEntry(m))
	}
	resp.SearchResult3 = sr
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

// --- stream: 3-tier pipeline ----------------------------------------------

// Stream implements GET /rest/stream.view?id=... Each tier is tried in
// order until one serves audio:
//
// Tier 0: local music library (Lidarr output, instant).
// Tier 1: local disk cache (instant, Range-capable).
// Tier 2: AllDebrid cloud (unlock + proxy, save to cache).
// Tier 3: Soulseek direct via slskd (music-native, often fastest).
// Tier 4: debrid scrape, last resort — Prowlarr, then public indexes,
// each fully resolved (search -> upload -> Ready) before the next.
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

	// Tier 0: local music library (e.g. Lidarr's completed downloads).
	// Authoritative full files, instant, Range-capable.
	if s.library != nil {
		if path, ok := s.library.Lookup(artist, album, title); ok {
			log.Printf("stream: %q - %q: tier=library", artist, title)
			s.serveCached(w, r, path)
			return
		}
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
		// Another goroutine is fetching (possibly a slow Soulseek
		// download). Wait for the cache to fill — the fetch writes it
		// there on success — bounded by the client's own request context.
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

	// Tier 2: already in the user's AllDebrid cloud?
	ctx := r.Context()
	if mg, file, ok := s.debrid.FindInCloud(ctx, artist, album, title); ok {
		log.Printf("stream: %q - %q: tier=debrid cloud (%s / %s)", artist, title, mg.Filename, file.Filename)
		if s.proxyUnlocked(w, r, id, mg, file) {
			return
		}
		log.Printf("stream: %q - %q: debrid unlock/proxy failed, trying scrape", artist, title)
		// Fall through to Tier 3 on unlock/proxy failure.
	} else {
		log.Printf("stream: %q - %q: not in debrid cloud", artist, title)
	}

	// Tier 3: scrape an index, upload the magnet, wait Ready, unlock, proxy.
	// Prowlarr first when configured (private indexers, best quality), then
	// the built-in public indexes (no setup needed). AllDebrid itself has no
	// search API — it can only download magnets something else found.
	// Album releases are preferred over single-track torrents throughout:
	// better seeded, better quality, and one upload makes every track on
	// the album instantly playable via Tier 2 afterwards. Track IDs often
	// name a single release (album == title), so resolve every plausible
	// album first and keep track-level queries as last resort.
	albumsToTry := s.albumCandidates(ctx, artist, album, title)
	log.Printf("stream: %q - %q: album candidates %q", artist, title, albumsToTry)
	var queries []string
	addQuery := func(q string) {
		q = strings.TrimSpace(strings.Join(strings.Fields(q), " "))
		if q == "" {
			return
		}
		for _, have := range queries {
			if have == q {
				return
			}
		}
		queries = append(queries, q)
	}
	for _, al := range albumsToTry {
		clean := stripBrackets(al)
		addQuery(artist + " " + clean + " FLAC")
		addQuery(artist + " " + clean)
	}
	addQuery(artist + " " + title)
	addQuery(title + " FLAC")
	primaryAlbum := ""
	if len(albumsToTry) > 0 {
		primaryAlbum = albumsToTry[0]
	}
	// Tier 3: Soulseek direct (music-native network, often faster than DHT
	// for audio). Needs slskd configured (SLSKD_URL/SLSKD_DOWNLOAD_DIR).
	// Failure falls through to the debrid scrape below.
	if s.soulseek != nil && s.soulseek.Enabled() {
		if path, err := s.soulseek.FetchTrackShared(ctx, artist, album, title); err != nil {
			log.Printf("stream: %q - %q: soulseek failed (%v), trying debrid scrape", artist, title, err)
		} else {
			log.Printf("stream: %q - %q: tier=soulseek", artist, title)
			s.serveSoulseek(w, r, id, path)
			return
		}
	}

	// Tier 4: debrid scrape, last resort. Each backend is fully resolved
	// (search -> upload -> Ready) before the next is tried, so a dead
	// Prowlarr result set or a failed upload never blocks the public
	// fallback. AllDebrid itself has no search API — it can only download
	// magnets something else found.
	type indexBackend struct {
		name    string
		enabled bool
		queries []string
		search  func(ctx context.Context, q string) ([]string, error)
	}
	pubQueries := queries
	if len(pubQueries) > 3 {
		pubQueries = pubQueries[:3] // bound latency on slow public APIs
	}
	backends := []indexBackend{
		{name: "prowlarr", enabled: s.indexer != nil && s.indexer.Enabled(), queries: queries, search: func(c context.Context, q string) ([]string, error) {
			return s.indexer.Search(c, q, 5)
		}},
		{name: "public", enabled: s.public != nil, queries: pubQueries, search: func(c context.Context, q string) ([]string, error) {
			return s.public.Search(c, q, 5)
		}},
	}
	for _, b := range backends {
		if !b.enabled {
			continue
		}
		// Try specific -> general queries: edition tags in parens ("(Revised
		// International Deluxe)") routinely match nothing on indexers, while a
		// plain "artist album" query hits the same torrents.
		var candidates []string
		picked := ""
		for _, q := range b.queries {
			sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			c, err := b.search(sctx, q)
			cancel()
			if err != nil {
				log.Printf("stream: %q - %q: %s %q: %v", artist, title, b.name, q, err)
				continue
			}
			if len(c) > 0 {
				candidates, picked = c, q
				break
			}
		}
		if len(candidates) == 0 {
			log.Printf("stream: %q - %q: %s returned no results, trying next", artist, title, b.name)
			continue
		}
		log.Printf("stream: %q - %q: index %q via %s -> %d candidate(s)", artist, title, picked, b.name, len(candidates))
		candidates = debrid.PreferAlbumTorrents(candidates, primaryAlbum)
		for i, cnd := range candidates {
			head := cnd
			if len(head) > 150 {
				head = head[:150]
			}
			log.Printf("stream: candidate[%d] len=%d head=%q", i, len(cnd), head)
		}
		if s.serveDebridCandidates(w, r, id, artist, title, b.name, candidates) {
			return
		}
	}
	log.Printf("stream: %q - %q: FAILED (no source resolved)", artist, title)
	writeFail(w, r, 70, "No index results for \""+artist+" "+album+"\"")
}

// serveDebridCandidates uploads one backend's candidates, waits for Ready,
// picks the track audio and proxies it. It reports true when the response
// is complete (or terminally broken mid-stream, after headers went out),
// false when the next source should be tried (upload failed or the ready
// torrent holds no playable audio — both pre-header failures).
func (s *Server) serveDebridCandidates(w http.ResponseWriter, r *http.Request, id, artist, title, via string, candidates []string) bool {
	uctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	mg, err := s.debrid.EnsureSeededUpload(uctx, candidates, 90*time.Second)
	if err != nil {
		log.Printf("stream: %q - %q: %s upload failed (%v), trying next", artist, title, via, err)
		return false
	}
	file, ok := debrid.PickAudioFile(mg.Links, title)
	if !ok {
		log.Printf("stream: %q - %q: no audio in %q, trying next", artist, title, mg.Filename)
		return false
	}
	log.Printf("stream: %q - %q: tier=scrape (%s / %s)", artist, title, mg.Filename, file.Filename)
	if !s.proxyUnlocked(w, r, id, mg, file) {
		log.Printf("stream: %q - %q: FAILED (scraped proxy failed)", artist, title)
		writeFail(w, r, 0, "Streaming failed for \""+title+"\"")
	}
	return true
}

// proxyUnlocked unlocks file.Link and proxies bytes to the client while
// filling the disk cache on full (non-Range) requests.
func (s *Server) proxyUnlocked(w http.ResponseWriter, r *http.Request, id string, mg debrid.Magnet, file debrid.MagnetFile) bool {
	ctx := r.Context()
	lctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	direct, err := s.debrid.UnlockLink(lctx, file.Link)
	cancel()
	if err != nil {
		return false
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodGet, direct, nil)
	if err != nil {
		return false
	}
	if rng := r.Header.Get("Range"); rng != "" {
		upReq.Header.Set("Range", rng) // preserve seeking semantics upstream
	}
	upReq.Header.Set("User-Agent", "DebridSubsonic/1.0")
	upResp, err := s.proxy.Do(upReq)
	if err != nil {
		return false
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK && upResp.StatusCode != http.StatusPartialContent {
		return false
	}

	ctype := upResp.Header.Get("Content-Type")
	if ctype == "" || strings.HasPrefix(ctype, "text/") {
		ctype = contentTypeFor(file.Filename)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	if cl := upResp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if cr := upResp.Header.Get("Content-Range"); cr != "" {
		w.Header().Set("Content-Range", cr)
	}
	w.WriteHeader(upResp.StatusCode)

	// Range (seek) requests: pure proxy + background full fill for next time.
	if upResp.StatusCode == http.StatusPartialContent {
		_, _ = io.CopyBuffer(w, upResp.Body, make([]byte, 32<<10))
		go s.backgroundFill(id, direct, file.Filename)
		return true
	}

	// Full request: TeeReader streams to the client while spilling to a
	// temp file; commit to cache only on clean EOF (no partial files).
	tmp, err := os.CreateTemp(s.storage.Dir(), id+".*.tmp")
	if err != nil {
		_, _ = io.CopyBuffer(w, upResp.Body, make([]byte, 32<<10))
		return true
	}
	tmpPath := tmp.Name()
	tee := io.TeeReader(upResp.Body, tmp)
	_, copyErr := io.CopyBuffer(w, tee, make([]byte, 32<<10))
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return true // client already got what it got; just skip caching
	}
	_ = s.storage.CommitTemp(id, tmpPath)
	return true
}

// backgroundFill fetches the whole file (no Range) for future instant seeks.
func (s *Server) backgroundFill(id, directURL, filename string) {
	if _, ok := s.storage.Lookup(id); ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "DebridSubsonic/1.0")
	resp, err := s.proxy.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	// Bounded: refuse absurd bodies (>2 GiB single track).
	_ = s.storage.Store(id, io.LimitReader(resp.Body, 2<<30))
}

// serveSoulseek promotes a Soulseek-downloaded file into the disk cache
// (so retries, concurrent requesters and Range seeks all converge on a
// normal cache entry) and serves it. If the copy into the cache fails the
// file is served in place rather than failing playback.
func (s *Server) serveSoulseek(w http.ResponseWriter, r *http.Request, id, path string) {
	if f, err := os.Open(path); err == nil {
		if serr := s.storage.Store(id, f); serr != nil {
			log.Printf("stream: cache store %s: %v", id, serr)
		}
		_ = f.Close()
	} else {
		log.Printf("stream: open soulseek file %s: %v", path, err)
	}
	if cpath, ok := s.storage.Lookup(id); ok {
		s.serveCached(w, r, cpath)
		return
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

// albumCandidates returns album names worth scraping for a track: the ID's
// own album (unless it looks like a single release, e.g. album == title)
// plus up to two distinct albums from a metadata lookup. Whole-album
// torrents beat single-track rips on seeders, quality, and future Tier-2
// hits, so track IDs that merely name a single get resolved to the real
// album here. Always returns at least the ID album as last resort when set.
func (s *Server) albumCandidates(ctx context.Context, artist, album, title string) []string {
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	found, err := s.meta.Search(ectx, strings.TrimSpace(artist+" "+title), 10)
	if err != nil {
		found = nil
	}
	return rankAlbumCandidates(album, found, artist, title)
}

// rankAlbumCandidates is the pure ranking core of albumCandidates: ID album
// first (unless single-like), then distinct lookup albums attached to the
// track, capped at three. Falls back to the ID album so a query chain
// always exists when the ID named anything.
func rankAlbumCandidates(idAlbum string, found []metadata.Song, artist, title string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || seen[strings.ToLower(a)] {
			return
		}
		seen[strings.ToLower(a)] = true
		out = append(out, a)
	}
	if !isSingleLike(idAlbum, title) {
		add(idAlbum)
	}
	for _, f := range found {
		if len(out) >= 3 {
			break
		}
		if f.Album == "" || isSingleLike(f.Album, f.Title) {
			continue
		}
		titleHit := strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(title))
		artistHit := strings.TrimSpace(artist) != "" &&
			strings.Contains(strings.ToLower(f.Artist), strings.ToLower(strings.TrimSpace(artist)))
		if !titleHit && !artistHit {
			continue
		}
		add(f.Album)
	}
	if len(out) == 0 && strings.TrimSpace(idAlbum) != "" {
		add(idAlbum) // single-like but nothing better: keep a queryable album
	}
	return out
}

// isSingleLike reports whether an album name looks like a single release
// rather than a real album: empty, identical to the track title, or
// explicitly tagged as a single.
func isSingleLike(album, title string) bool {
	a := strings.TrimSpace(album)
	if a == "" {
		return true
	}
	if strings.EqualFold(a, strings.TrimSpace(title)) {
		return true
	}
	return strings.Contains(strings.ToLower(a), "single")
}

// stripBrackets removes (...) and [...] groups from s.
func stripBrackets(s string) string {
	for {
		start := strings.IndexAny(s, "([")
		if start == -1 {
			break
		}
		closer := ")"
		if s[start] == '[' {
			closer = "]"
		}
		rest := s[start+1:]
		end := strings.Index(rest, closer)
		if end == -1 {
			break
		}
		s = s[:start] + " " + rest[end+1:]
	}
	return strings.Join(strings.Fields(s), " ")
}

func contentTypeFor(filename string) string {
	l := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(l, ".flac"):
		return "audio/flac"
	case strings.HasSuffix(l, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(l, ".m4a"), strings.HasSuffix(l, ".aac"):
		return "audio/mp4"
	case strings.HasSuffix(l, ".ogg"), strings.HasSuffix(l, ".opus"):
		return "audio/ogg"
	case strings.HasSuffix(l, ".wav"):
		return "audio/wav"
	default:
		return "audio/mpeg"
	}
}
