package subsonic

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/audioinfo"
	"github.com/musicstreamer/musicstreamer/pkg/cache"
	"github.com/musicstreamer/musicstreamer/pkg/library"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
	"github.com/musicstreamer/musicstreamer/pkg/soulseek"
	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
)

// Server wires config + collaborators to Subsonic HTTP handlers.
type Server struct {
	user     string
	pass     string
	meta     *metadata.Client
	soulseek *soulseek.Client
	yt       *ytaudio.Client
	storage  *cache.Manager
	preview  *cache.Manager
	artwork  *cache.Manager
	lib      *library.Store

	// proxy performs upstream GETs (cover art, YouTube audio previews). No
	// client-level Timeout: the request context governs each fetch.
	proxy *http.Client

	// fetchFlight coalesces background canonical fetches (one per song id).
	fetchFlight flight

	// previewFlight coalesces concurrent YouTube preview lookups per song id,
	// and previewURLs remembers fresh resolved URLs briefly so a client that
	// aborts its request mid-lookup (a probe/retry) gets the next one instantly.
	previewFlight flight
	previewMu     sync.Mutex
	previewURLs   map[string]previewURL
	previewFail   map[string]time.Time

	// warmSem bounds concurrent background pre-resolutions so browsing a list
	// cannot spawn one yt-dlp process per track.
	warmSem chan struct{}

	// pins remembers, per client connection, which file variant a track's
	// playback began on, so seeks stay byte-consistent (see singleflight.go).
	pinMu sync.Mutex
	pins  map[string]pinEntry

	// info memoizes probed audio properties (format/sample rate/bit depth)
	// per cached song id; size invalidates the entry on re-download.
	infoMu sync.Mutex
	info   map[string]infoCache

	// Artwork resolution is cached on disk and coalesced per cover key.
	coverMu     sync.Mutex
	coverThumbs map[string]string
	coverFail   map[string]coverFailure
	coverJobs   map[string]*coverJob
	coverSem    chan struct{}

	// Native API bearer tokens (issued by /auth/login).
	nativeMu     sync.Mutex
	nativeTokens map[string]nativeSession
}

type coverFailure struct {
	status int
	exp    time.Time
}

type coverJob struct {
	done   chan struct{}
	status int
}

// infoCache stores a probed track's properties alongside the file size they
// were derived from.
type infoCache struct {
	size int64
	info audioinfo.Info
}

// NewServer builds a handler set. proxy may be nil (a default is used).
// sk and yt may be nil (disables the Soulseek and YouTube preview sources).
// preview may be nil (disables lossy instant-playback caching).
// artwork may be nil (cover requests then fail with 503).
func NewServer(user, pass string, m *metadata.Client, sk *soulseek.Client, yt *ytaudio.Client, s *cache.Manager, preview, artwork *cache.Manager, lib *library.Store, proxy *http.Client) *Server {
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
		meta: m, soulseek: sk, yt: yt, storage: s, preview: preview, artwork: artwork, lib: lib,
		proxy:    proxy,
		warmSem:  make(chan struct{}, 2),
		coverSem: make(chan struct{}, 4),
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
	resp.License = &License{Valid: true, Email: "musicstreamer@local", Key: "MusicStreamer", Date: "2030-01-01"}
	write(w, r, resp)
}

func (s *Server) MusicFolders(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.MusicFolders = &MusicFolders{Folders: []Folder{{ID: 0, Name: "Music"}}}
	write(w, r, resp)
}

// GetUser answers Feishin's login probe. The single configured user gets
// full playback/playlist rights; upload/admin-only flags stay off.
func (s *Server) GetUser(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	user := s.userEntry()
	resp.User = &user
	write(w, r, resp)
}

// albumList returns cached albums ordered per the Subsonic `type` parameter.
// Only the cache is browsable; ordering that needs play history (newest,
// recent, frequent) falls back to case-insensitive album name.
func (s *Server) albumList(r *http.Request) []AlbumEntry {
	_ = r.ParseForm()
	typ := strings.ToLower(firstOf(r.Form, "type"))
	size := atoiWithDefault(firstOf(r.Form, "size"), 500)
	if size > 500 {
		size = 500
	}
	offset := atoiWithDefault(firstOf(r.Form, "offset"), 0)
	if offset < 0 {
		offset = 0
	}
	cat := s.catalog()
	var albums []AlbumEntry
	switch typ {
	case "starred":
		if s.lib != nil {
			for _, id := range s.lib.StarredAlbums() {
				if e, ok := s.albumEntryForID(id); ok {
					albums = append(albums, e)
				}
			}
		}
		return pageAlbums(albums, offset, size)
	case "random":
		albums = cat.albumEntries()
		rand.Shuffle(len(albums), func(i, j int) {
			albums[i], albums[j] = albums[j], albums[i]
		})
		return pageAlbums(albums, offset, size)
	default:
		albums = cat.albumEntries()
	}
	sort.Slice(albums, func(i, j int) bool {
		if typ == "alphabeticalbyartist" && !strings.EqualFold(albums[i].Artist, albums[j].Artist) {
			return strings.ToLower(albums[i].Artist) < strings.ToLower(albums[j].Artist)
		}
		return strings.ToLower(albums[i].Name) < strings.ToLower(albums[j].Name)
	})
	return pageAlbums(albums, offset, size)
}

func pageAlbums(albums []AlbumEntry, offset, size int) []AlbumEntry {
	if offset > len(albums) {
		offset = len(albums)
	}
	end := offset + size
	if end > len(albums) {
		end = len(albums)
	}
	return albums[offset:end]
}

// AlbumList serves the pre-ID3 album list (cache-backed; see catalog.go).
func (s *Server) AlbumList(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.AlbumList = &AlbumList{Albums: s.albumList(r)}
	write(w, r, resp)
}

func (s *Server) AlbumList2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.AlbumList2 = &AlbumList2{Albums: s.albumList(r)}
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
		s.prewarmPage(info.Songs)
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
	s.prewarmPage(info.Songs)
	write(w, r, resp)
}

// CoverArt serves artwork for a song/album/artist ID. Artwork is resolved
// once (metadata first, then the track's YouTube thumbnail), validated, and
// cached on disk. Importantly, failures use real HTTP error statuses: clients
// such as Psysonic cache any 2xx body as an image and would otherwise keep a
// broken XML response forever.
func (s *Server) CoverArt(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	s.serveCover(w, r, firstOf(r.Form, "id"))
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

func (s *Server) Indexes(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Artists = &Artists{
		Ignored: "The El La Los Las Le Les",
		Index:   indexGroups(s.catalog().artistEntries()),
	}
	write(w, r, resp)
}

func (s *Server) OpenExtensions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Extensions = &Extensions{List: []Extension{
		{Name: "search3", Versions: []int{1}},
		{Name: "formPost", Versions: []int{1}},
		{Name: "songLyrics", Versions: []int{1}},
	}}
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

// searchAll resolves the requested page of search results plus the distinct
// albums/artists those songs belong to. An empty query means "the whole cached
// library" (clients page it via songOffset/songCount); a non-empty query
// returns cached matches first, then metadata-search results.
func (s *Server) searchAll(r *http.Request) (*SearchResult, error) {
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
			return nil, err
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
	page := entries[offset:end]

	res := &SearchResult{Songs: page}
	seenAlbums := make(map[string]bool)
	seenArtists := make(map[string]bool)
	for _, e := range page {
		if e.AlbumID != "" && !seenAlbums[e.AlbumID] {
			seenAlbums[e.AlbumID] = true
			if ae, ok := s.albumEntryForID(e.AlbumID); ok {
				res.Albums = append(res.Albums, ae)
			}
		}
		if e.ArtistID != "" && !seenArtists[e.ArtistID] {
			seenArtists[e.ArtistID] = true
			if ae, ok := s.artistEntryForID(e.ArtistID); ok {
				res.Artists = append(res.Artists, ae)
			}
		}
	}
	return res, nil
}

func (s *Server) Search3(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	res, err := s.searchAll(r)
	if err != nil {
		writeFail(w, r, 0, "search failed: "+err.Error())
		return
	}
	resp := okResponse()
	resp.SearchResult3 = res
	s.prewarmPage(res.Songs)
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
	s.prewarmPage(songs)
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
	if s.lib != nil {
		e.PlayCount = s.lib.PlayCount(id)
		e.UserRating = s.lib.Rating(id)
	}
	resp.Song = &e
	s.prewarmPage([]SongEntry{e})
	write(w, r, resp)
}

// --- stream: cache -> Soulseek pipeline -----------------------------------

// Stream implements GET /rest/stream.view?id=... Two sources, tried in
// Stream implements GET /rest/stream.view?id=...
//
// Sources, in order:
//
//   - Full disk cache (lossless/canonical): instant, Range-capable. Only
//     served for a *fresh* media load (no Range, or bytes=0-), because
//     switching files mid-playback would invalidate the client's byte offsets.
//   - Preview disk cache / YouTube preview: a small lossy m4a served instantly
//     while the full file is fetched in the background.
//   - Soulseek (blocking): the last resort when no preview can be resolved.
//
// A background fetch (Soulseek lossless first, then YouTube best-audio) fills
// the full cache; the next time the track is loaded it is served lossless.
// Mid-session Range requests stay pinned to the variant the track started on.
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

	fresh := rangeIsFresh(r)

	if fresh {
		// Tier 1: full cache. A fresh load always re-reads the header, so it is
		// safe to switch from a preview to the lossless file here.
		if path, ok := s.storage.Lookup(id); ok {
			log.Printf("stream: %q - %q: tier=cache", artist, title)
			s.pin(streamClient(r), id, variantFull)
			s.serveCached(w, r, path)
			return
		}
	} else {
		// Mid-session Range (seek): keep serving the variant this playback
		// began with so byte offsets remain valid.
		switch s.pinnedVariant(streamClient(r), id) {
		case variantFull:
			if path, ok := s.storage.Lookup(id); ok {
				s.serveCached(w, r, path)
				return
			}
			writeRangeUnavailable(w)
			return
		case variantPreview:
			if s.preview != nil {
				if path, ok := s.preview.Lookup(id); ok {
					s.serveCached(w, r, path)
					return
				}
			}
			writeRangeUnavailable(w)
			return
		}
		// Unknown/expired pin: best available (a rare offset reset).
		if path, ok := s.storage.Lookup(id); ok {
			s.pin(streamClient(r), id, variantFull)
			s.serveCached(w, r, path)
			return
		}
	}

	// Tier 2: cached preview (lossy, instant).
	if s.preview != nil {
		if path, ok := s.preview.Lookup(id); ok {
			log.Printf("stream: %q - %q: tier=preview-cache", artist, title)
			s.pin(streamClient(r), id, variantPreview)
			s.serveCached(w, r, path)
			go s.startFullFetch(id, artist, album, title)
			return
		}
	}

	// Tier 3: resolve a fresh YouTube preview and stream it now.
	if s.yt != nil && s.yt.Enabled() {
		// A URL resolved moments ago (possibly for a request the client
		// abandoned) skips yt-dlp entirely.
		if st, ok := s.cachedPreviewURL(id); ok {
			log.Printf("stream: %q - %q: tier=youtube preview (recent url)", artist, title)
			s.pin(streamClient(r), id, variantPreview)
			s.servePreview(w, r, id, st)
			go s.startFullFetch(id, artist, album, title)
			return
		}

		if s.previewFlight.begin(id) {
			// Detached from the client context: a client that gives up early
			// (common on a cold YouTube lookup) must not abort a resolve that
			// is nearly done — the result is still cached for its retry.
			rctx, rcancel := context.WithTimeout(context.Background(), 90*time.Second)
			st, rerr := s.yt.Resolve(rctx, artist, title, false)
			rcancel()
			if rerr == nil {
				s.rememberPreviewURL(id, st)
			}
			s.previewFlight.finish(id)
			if rerr == nil {
				s.pin(streamClient(r), id, variantPreview)
				if r.Context().Err() != nil {
					log.Printf("stream: %q - %q: client left during resolve; caching preview for the retry", artist, title)
					go s.cachePreview(id, st)
				} else {
					log.Printf("stream: %q - %q: tier=youtube preview", artist, title)
					s.servePreview(w, r, id, st)
				}
				go s.startFullFetch(id, artist, album, title)
				return
			}
			log.Printf("stream: %q - %q: youtube resolve failed: %v", artist, title, rerr)
			s.markPreviewFailed(id)
		} else if st, ok := s.awaitPreviewURL(r.Context(), id, 15*time.Second); ok {
			// Another request is already resolving this track; reuse it
			// instead of starting a duplicate yt-dlp run.
			log.Printf("stream: %q - %q: tier=youtube preview (coalesced)", artist, title)
			s.pin(streamClient(r), id, variantPreview)
			s.servePreview(w, r, id, st)
			go s.startFullFetch(id, artist, album, title)
			return
		}
	}

	// Tier 4: blocking Soulseek — no preview available.
	if s.soulseek != nil && s.soulseek.Enabled() {
		if path, serr := s.soulseek.FetchTrackShared(r.Context(), artist, album, title); serr != nil {
			log.Printf("stream: %q - %q: soulseek failed: %v", artist, title, serr)
		} else {
			log.Printf("stream: %q - %q: tier=soulseek", artist, title)
			s.pin(streamClient(r), id, variantFull)
			s.serveSoulseek(w, r, id, path)
			return
		}
	}
	log.Printf("stream: %q - %q: FAILED (no source resolved)", artist, title)
	writeFail(w, r, 70, "No source for \""+artist+" - "+title+"\"")
}

// servePreview proxies a resolved YouTube audio URL to the client, preserving
// Range/seek semantics. On a full (200) response it also spills the bytes to a
// temp file and commits them to the preview cache, so replays start instantly.
func (s *Server) servePreview(w http.ResponseWriter, r *http.Request, id string, st ytaudio.Stream) {
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(r.Context(), method, st.URL, nil)
	if err != nil {
		writeFail(w, r, 0, "Stream unavailable")
		return
	}
	rng := r.Header.Get("Range")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", "MusicStreamer/1.0")
	resp, err := s.proxy.Do(req)
	if err != nil {
		log.Printf("stream: preview %s: %v", id, err)
		writeFail(w, r, 0, "Stream unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			w.Header().Set("Content-Range", cr)
		}
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		writeFail(w, r, 0, "Stream unavailable")
		return
	}

	ctype := st.ContentType
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		ctype = ct
	}
	if ctype == "" || strings.HasPrefix(ctype, "text/") {
		ctype = "audio/mp4"
	}

	// The upstream ignored a mid-stream Range and returned the whole file. We
	// must never relay a 200 for a non-zero offset: players reject it and the
	// seek dies. Cache the full preview, then let ServeContent answer the range.
	if resp.StatusCode == http.StatusOK && rng != "" && !rangeIsFresh(r) {
		if method == http.MethodHead {
			writeRangeUnavailable(w)
			return
		}
		path, ok := s.storePreviewBody(id, resp.Body)
		if !ok {
			writeRangeUnavailable(w)
			return
		}
		s.serveCached(w, r, path)
		return
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		w.Header().Set("Content-Range", cr)
	}
	w.WriteHeader(resp.StatusCode)
	if method == http.MethodHead {
		return
	}

	// Range request (seek): pure proxy, never cache a partial file.
	if resp.StatusCode == http.StatusPartialContent || s.preview == nil {
		_, _ = io.CopyBuffer(w, resp.Body, make([]byte, 32<<10))
		return
	}

	// Full GET: stream to the client while spilling to a temp file; commit to
	// the preview cache only on a clean EOF (no partial files).
	if _, ok := s.storePreviewTee(id, w, resp.Body); !ok {
		return
	}
}

// storePreviewBody drains a full upstream response into the preview cache.
func (s *Server) storePreviewBody(id string, body io.Reader) (string, bool) {
	if s.preview == nil {
		return "", false
	}
	if _, ok := s.storePreviewTee(id, nil, body); !ok {
		return "", false
	}
	return s.preview.Lookup(id)
}

// storePreviewTee spools body to a temp file, optionally echoing it to dst, and
// commits it only after a clean EOF.
func (s *Server) storePreviewTee(id string, dst io.Writer, body io.Reader) (string, bool) {
	if s.preview == nil {
		return "", false
	}
	tmp, err := os.CreateTemp(s.preview.Dir(), id+".*.tmp")
	if err != nil {
		if dst != nil {
			_, _ = io.CopyBuffer(dst, body, make([]byte, 32<<10))
		}
		return "", false
	}
	tmpPath := tmp.Name()
	var src io.Reader = body
	if dst != nil {
		src = io.TeeReader(body, tmp)
	}
	var copyErr error
	if dst != nil {
		_, copyErr = io.CopyBuffer(dst, src, make([]byte, 32<<10))
	} else {
		_, copyErr = io.CopyBuffer(tmp, src, make([]byte, 32<<10))
	}
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", false
	}
	if err := s.preview.CommitTemp(id, tmpPath); err != nil {
		log.Printf("stream: preview cache store %s: %v", id, err)
		return "", false
	}
	path, ok := s.preview.Lookup(id)
	return path, ok
}

func writeRangeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	http.Error(w, "Range unavailable", http.StatusServiceUnavailable)
}

// cachePreview downloads a resolved preview URL in full and commits it to the
// preview cache. Used when the requesting client disconnects mid-resolve, so
// its retry (or the next click) starts instantly instead of re-running yt-dlp.
func (s *Server) cachePreview(id string, st ytaudio.Stream) {
	if s.preview == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "MusicStreamer/1.0")
	resp, err := s.proxy.Do(req)
	if err != nil {
		log.Printf("stream: preview warm %s: %v", id, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	tmp, err := os.CreateTemp(s.preview.Dir(), id+".*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := s.preview.CommitTemp(id, tmpPath); err != nil {
		log.Printf("stream: preview warm store %s: %v", id, err)
	}
}

// prewarmPerPage caps how many listed tracks are pre-resolved per browse
// request. Kept small: each one is a full yt-dlp search+extract.
const prewarmPerPage = 6

// prewarmPage resolves preview URLs for the first few tracks of a list the
// client is about to display. By the time the user presses play, the URL is
// usually ready, so playback starts immediately instead of paying yt-dlp's
// latency in the stream request (which some clients abort while waiting).
// Only the URL is memoized; the preview file caches on first play.
func (s *Server) prewarmPage(songs []SongEntry) {
	if s.yt == nil || !s.yt.Enabled() || s.warmSem == nil {
		return
	}
	n := len(songs)
	if n > prewarmPerPage {
		n = prewarmPerPage
	}
	for i := 0; i < n; i++ {
		s.prewarmPreview(songs[i])
	}
}

func (s *Server) prewarmPreview(e SongEntry) {
	id := e.ID
	if id == "" || strings.TrimSpace(e.Title) == "" {
		return
	}
	if s.storage != nil {
		if _, ok := s.storage.Lookup(id); ok {
			return
		}
	}
	if s.preview != nil {
		if _, ok := s.preview.Lookup(id); ok {
			return
		}
	}
	if _, ok := s.cachedPreviewURL(id); ok {
		return
	}
	if s.previewFailed(id) {
		return
	}
	go func() {
		s.warmSem <- struct{}{}
		defer func() { <-s.warmSem }()
		// Re-check: another request may have resolved it while we queued.
		if _, ok := s.cachedPreviewURL(id); ok {
			return
		}
		if s.preview != nil {
			if _, ok := s.preview.Lookup(id); ok {
				return
			}
		}
		if s.previewFailed(id) {
			return
		}
		if !s.previewFlight.begin(id) {
			return
		}
		defer s.previewFlight.finish(id)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		st, err := s.yt.Resolve(ctx, e.Artist, e.Title, false)
		if err != nil {
			log.Printf("prewarm: %q - %q: %v", e.Artist, e.Title, err)
			s.markPreviewFailed(id)
			return
		}
		s.rememberPreviewURL(id, st)
		log.Printf("prewarm: %q - %q: url ready", e.Artist, e.Title)
	}()
}

// startFullFetch fetches the canonical (lossless-preferred) file into the full
// cache in the background, once per song id. Soulseek is tried first; when it
// yields nothing, YouTube's best audio is cached instead. The preview entry is
// left in place (it ages out via the preview LRU).
func (s *Server) startFullFetch(id, artist, album, title string) {
	if !s.fetchFlight.begin(id) {
		return // already being fetched
	}
	defer s.fetchFlight.finish(id)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Preferred: Soulseek (FLAC when available).
	if s.soulseek != nil && s.soulseek.Enabled() {
		path, err := s.soulseek.FetchTrackShared(ctx, artist, album, title)
		if err != nil {
			log.Printf("fetch: %q - %q: soulseek failed: %v", artist, title, err)
		} else if f, oerr := os.Open(path); oerr == nil {
			serr := s.storage.Store(id, f)
			f.Close()
			if serr == nil {
				log.Printf("fetch: %q - %q: cached (soulseek)", artist, title)
				return
			}
			log.Printf("fetch: %q - %q: store: %v", artist, title, serr)
		}
	}

	// Fallback: YouTube best audio as the canonical file.
	if s.yt != nil && s.yt.Enabled() {
		st, err := s.yt.Resolve(ctx, artist, title, true)
		if err != nil {
			log.Printf("fetch: %q - %q: youtube resolve failed: %v", artist, title, err)
			return
		}
		s.rememberThumbnail(id, st.ThumbnailURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.URL, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "MusicStreamer/1.0")
		resp, err := s.proxy.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			return
		}
		defer resp.Body.Close()
		if err := s.storage.Store(id, io.LimitReader(resp.Body, 512<<20)); err != nil {
			log.Printf("fetch: %q - %q: youtube store: %v", artist, title, err)
			return
		}
		log.Printf("fetch: %q - %q: cached (youtube best audio)", artist, title)
	}
}

// serveSoulseek serves a completed Soulseek file immediately and promotes it
// into the full disk cache in the background.
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

// rangeIsFresh reports whether the request is the start of a new media load:
// no Range header, or a range beginning at byte 0. A prefix range that starts
// at 0 re-reads the header, so switching files on it is safe; any other range
// (bytes=N-, N>0, or a suffix range) continues an existing load.
func rangeIsFresh(r *http.Request) bool {
	spec := strings.TrimSpace(r.Header.Get("Range"))
	if spec == "" {
		return true
	}
	if !strings.HasPrefix(spec, "bytes=") {
		return true
	}
	spec = strings.TrimPrefix(spec, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i] // first range only
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return true
	}
	start := strings.TrimSpace(spec[:dash])
	if start == "" {
		return false // suffix range (bytes=-N): tail of an existing load
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return true
	}
	return n <= 0
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
