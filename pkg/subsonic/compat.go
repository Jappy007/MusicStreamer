package subsonic

// Navidrome/client compatibility surface: persistent playlists, stars,
// ratings, scrobbling/now-playing, play queue, scan status, artist/album info
// and the remaining endpoints clients probe. State lives in pkg/library; song
// identity stays stateless (reversible IDs), so every listed track remains
// playable through the cache -> preview -> Soulseek pipeline.

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/library"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
)

// --- playlists ------------------------------------------------------------

func (s *Server) playlistCover(p library.Playlist) string {
	if len(p.SongIDs) > 0 {
		return p.SongIDs[0]
	}
	return ""
}

func (s *Server) playlistSummary(p library.Playlist) Playlist {
	duration := 0
	for _, id := range p.SongIDs {
		if e, ok := s.entryForID(id); ok {
			duration += e.Duration
		}
	}
	return Playlist{
		ID: p.ID, Name: p.Name, Comment: p.Comment, Owner: p.Owner,
		Public: p.Public, SongCount: len(p.SongIDs), Duration: duration,
		CoverArt: s.playlistCover(p),
		Created:  p.Created.Format(time.RFC3339),
		Changed:  p.Changed.Format(time.RFC3339),
	}
}

func (s *Server) playlistWithSongs(p library.Playlist) *PlaylistWithSongs {
	out := &PlaylistWithSongs{
		ID: p.ID, Name: p.Name, Comment: p.Comment, Owner: p.Owner,
		Public: p.Public, CoverArt: s.playlistCover(p),
		Created: p.Created.Format(time.RFC3339), Changed: p.Changed.Format(time.RFC3339),
	}
	for _, id := range p.SongIDs {
		if e, ok := s.entryForID(id); ok {
			out.Entries = append(out.Entries, e)
			out.Duration += e.Duration
		}
	}
	out.SongCount = len(out.Entries)
	return out
}

func (s *Server) GetPlaylists(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	list := &Playlists{}
	if s.lib != nil {
		ps := s.lib.Playlists()
		sort.Slice(ps, func(i, j int) bool {
			return strings.ToLower(ps[i].Name) < strings.ToLower(ps[j].Name)
		})
		for _, p := range ps {
			list.Items = append(list.Items, s.playlistSummary(p))
		}
	}
	resp.Playlists = list
	write(w, r, resp)
}

func (s *Server) GetPlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id", "playlistId")
	if s.lib == nil {
		writeFail(w, r, 70, "Playlist not found")
		return
	}
	p, ok := s.lib.Playlist(id)
	if !ok {
		writeFail(w, r, 70, "Playlist not found")
		return
	}
	resp := okResponse()
	resp.Playlist = s.playlistWithSongs(p)
	write(w, r, resp)
}

func (s *Server) CreatePlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib == nil {
		writeFail(w, r, 0, "Playlists unavailable")
		return
	}
	name := firstOf(r.Form, "name")
	id := firstOf(r.Form, "playlistId", "id")
	if name == "" && id == "" {
		writeFail(w, r, 10, "Missing playlist name")
		return
	}
	p := s.lib.CreatePlaylist(id, name, firstOf(r.Form, "comment"), s.user,
		boolParam(r.Form, "public", false), r.Form["songId"])
	resp := okResponse()
	resp.Playlist = s.playlistWithSongs(p)
	write(w, r, resp)
}

func (s *Server) UpdatePlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib == nil {
		writeFail(w, r, 0, "Playlists unavailable")
		return
	}
	id := firstOf(r.Form, "playlistId", "id")
	if id == "" {
		writeFail(w, r, 10, "Missing playlistId")
		return
	}
	var (
		name    *string
		comment *string
		public  *bool
	)
	if v, ok := formValue(r.Form, "name"); ok {
		name = &v
	}
	if v, ok := formValue(r.Form, "comment"); ok {
		comment = &v
	}
	if v, ok := formValue(r.Form, "public"); ok {
		b := boolValue(v, false)
		public = &b
	}
	if _, ok := s.lib.UpdatePlaylist(id, name, comment, public,
		r.Form["songIdToAdd"], intParams(r.Form["songIndexToRemove"])); !ok {
		writeFail(w, r, 70, "Playlist not found")
		return
	}
	write(w, r, okResponse())
}

func (s *Server) DeletePlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib != nil {
		s.lib.DeletePlaylist(firstOf(r.Form, "id", "playlistId"))
	}
	write(w, r, okResponse())
}

// --- stars / ratings ------------------------------------------------------

func (s *Server) starRequest(w http.ResponseWriter, r *http.Request, on bool) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib != nil {
		for _, id := range r.Form["id"] {
			s.lib.Star(library.KindSong, id, on)
		}
		for _, id := range r.Form["albumId"] {
			s.lib.Star(library.KindAlbum, id, on)
		}
		for _, id := range r.Form["artistId"] {
			s.lib.Star(library.KindArtist, id, on)
		}
	}
	write(w, r, okResponse())
}

func (s *Server) Star(w http.ResponseWriter, r *http.Request)   { s.starRequest(w, r, true) }
func (s *Server) Unstar(w http.ResponseWriter, r *http.Request) { s.starRequest(w, r, false) }

func (s *Server) SetRating(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib != nil {
		s.lib.SetRating(firstOf(r.Form, "id"), atoiWithDefault(firstOf(r.Form, "rating"), 0))
	}
	write(w, r, okResponse())
}

// starredLists resolves the stored starred IDs into response entries.
func (s *Server) starredLists() (songs []SongEntry, albums []AlbumEntry, artists []ArtistEntry) {
	if s.lib == nil {
		return
	}
	for _, id := range s.lib.StarredSongs() {
		if e, ok := s.entryForID(id); ok {
			songs = append(songs, e)
		}
	}
	for _, id := range s.lib.StarredAlbums() {
		if e, ok := s.albumEntryForID(id); ok {
			albums = append(albums, e)
		}
	}
	for _, id := range s.lib.StarredArtists() {
		if e, ok := s.artistEntryForID(id); ok {
			artists = append(artists, e)
		}
	}
	return
}

func (s *Server) GetStarred(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	songs, albums, artists := s.starredLists()
	resp := okResponse()
	resp.Starred = &Starred{Songs: songs, Albums: albums, Artists: artists}
	write(w, r, resp)
}

func (s *Server) GetStarred2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	songs, albums, artists := s.starredLists()
	resp := okResponse()
	resp.Starred2 = &Starred2{Songs: songs, Albums: albums, Artists: artists}
	write(w, r, resp)
}

// --- artist / album info --------------------------------------------------

// coverArtURL builds an absolute cover-art URL, carrying over the caller's
// auth params so a client can fetch it without re-authenticating.
func (s *Server) coverArtURL(r *http.Request, id string) string {
	if id == "" {
		return ""
	}
	q := url.Values{}
	q.Set("id", id)
	in := r.URL.Query()
	for _, k := range []string{"u", "t", "s", "p", "c", "v", "f"} {
		if v := in.Get(k); v != "" {
			q.Set(k, v)
		}
	}
	return s.absURL(r, "/rest/getCoverArt?"+q.Encode())
}

func (s *Server) absURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + path
}

func (s *Server) GetArtistInfo(w http.ResponseWriter, r *http.Request) {
	s.artistInfo(w, r, false)
}

func (s *Server) GetArtistInfo2(w http.ResponseWriter, r *http.Request) {
	s.artistInfo(w, r, true)
}

func (s *Server) artistInfo(w http.ResponseWriter, r *http.Request, id3 bool) {
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
	}
	if artist == "" {
		artist = title
	}
	if strings.TrimSpace(artist) == "" {
		writeFail(w, r, 70, "Artist not found")
		return
	}
	cover := s.coverArtURL(r, metadata.EncodeID(artist, "", ""))
	bio := &ArtistBio{
		SmallImageURL: cover, MediumImageURL: cover, LargeImageURL: cover,
	}
	resp := okResponse()
	if id3 {
		resp.ArtistInfo2 = bio
	} else {
		resp.ArtistInfo = bio
	}
	write(w, r, resp)
}

func (s *Server) GetAlbumInfo(w http.ResponseWriter, r *http.Request) {
	s.albumInfo(w, r, false)
}

func (s *Server) GetAlbumInfo2(w http.ResponseWriter, r *http.Request) {
	s.albumInfo(w, r, true)
}

func (s *Server) albumInfo(w http.ResponseWriter, r *http.Request, id3 bool) {
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
		album = title // tolerate a song id
	}
	if strings.TrimSpace(album) == "" {
		writeFail(w, r, 70, "Album not found")
		return
	}
	cover := s.coverArtURL(r, metadata.EncodeID(artist, album, ""))
	info := &AlbumBio{
		SmallImageURL: cover, MediumImageURL: cover, LargeImageURL: cover,
	}
	resp := okResponse()
	if id3 {
		resp.AlbumInfo2 = info
	} else {
		resp.AlbumInfo = info
	}
	write(w, r, resp)
}

// --- lyrics ---------------------------------------------------------------

func (s *Server) GetLyrics(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	resp := okResponse()
	resp.Lyrics = &Lyrics{
		Artist: firstOf(r.Form, "artist"),
		Title:  firstOf(r.Form, "title"),
	}
	write(w, r, resp)
}

func (s *Server) GetLyricsBySongId(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.LyricsList = &LyricsList{}
	write(w, r, resp)
}

// --- scrobbling / now playing --------------------------------------------

func (s *Server) Scrobble(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	submission := boolParam(r.Form, "submission", true)
	when := time.Now().UTC()
	if t := firstOf(r.Form, "time"); t != "" {
		if ms, err := strconv.ParseInt(t, 10, 64); err == nil && ms > 0 {
			when = time.UnixMilli(ms).UTC()
		}
	}
	ids := r.Form["id"]
	if len(ids) == 0 {
		if id := firstOf(r.Form, "id"); id != "" {
			ids = []string{id}
		}
	}
	if s.lib != nil {
		for _, id := range ids {
			s.lib.Scrobble(id, s.user, firstOf(r.Form, "c"), submission, when)
		}
	}
	write(w, r, okResponse())
}

func (s *Server) GetNowPlaying(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	out := &NowPlaying{}
	if s.lib != nil {
		for _, np := range s.lib.NowPlaying() {
			e, ok := s.entryForID(np.ID)
			if !ok {
				continue
			}
			entry := NowPlayingEntry{
				ID: e.ID, Parent: e.Parent, IsDir: e.IsDir, Title: e.Title,
				Album: e.Album, Artist: e.Artist, Track: e.Track,
				CoverArt: e.CoverArt, Duration: e.Duration,
				ContentType: e.ContentType, Suffix: e.Suffix, Size: e.Size,
				AlbumID: e.AlbumID, ArtistID: e.ArtistID, Type: e.Type,
				Username: np.User, PlayerName: np.Player,
				MinutesAgo: int(time.Since(np.Started).Minutes()),
			}
			if entry.MinutesAgo < 0 {
				entry.MinutesAgo = 0
			}
			out.Entries = append(out.Entries, entry)
		}
	}
	resp := okResponse()
	resp.NowPlaying = out
	write(w, r, resp)
}

// --- play queue -----------------------------------------------------------

func (s *Server) GetPlayQueue(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	pq := &PlayQueue{}
	if s.lib != nil {
		if q, ok := s.lib.Queue(); ok {
			pq.Current = q.Current
			pq.Position = q.Position
			pq.Username = q.User
			pq.Changed = q.Changed.Format(time.RFC3339)
			pq.ChangedBy = q.User
			for _, id := range q.SongIDs {
				if e, ok := s.entryForID(id); ok {
					pq.Entries = append(pq.Entries, e)
				}
			}
		}
	}
	resp.PlayQueue = pq
	write(w, r, resp)
}

func (s *Server) SavePlayQueue(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	if s.lib != nil {
		position, _ := strconv.ParseInt(firstOf(r.Form, "position"), 10, 64)
		s.lib.SaveQueue(library.PlayQueue{
			SongIDs:  r.Form["id"],
			Current:  firstOf(r.Form, "current"),
			Position: position,
			User:     s.user,
		})
	}
	write(w, r, okResponse())
}

// --- library scanning / browsing -----------------------------------------

func (s *Server) scanStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.ScanStatus = &ScanStatus{
		Scanning: false,
		Count:    int64(len(s.catalog().songs)),
	}
	write(w, r, resp)
}

func (s *Server) GetScanStatus(w http.ResponseWriter, r *http.Request) {
	s.scanStatus(w, r)
}

func (s *Server) StartScan(w http.ResponseWriter, r *http.Request) {
	s.scanStatus(w, r)
}

// GetIndexes serves getIndexes.view (the simulated /Artist/Album tree).
func (s *Server) GetIndexes(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Indexes = &Indexes{
		Ignored: "The El La Los Las Le Les",
		Index:   indexGroups(s.catalog().artistEntries()),
	}
	write(w, r, resp)
}

// indexGroups groups artists under their initial (A-Z, else #).
func indexGroups(artists []ArtistEntry) []Index {
	byLetter := make(map[string][]ArtistEntry)
	var letters []string
	for _, a := range artists {
		letter := indexLetter(a.Name)
		if _, ok := byLetter[letter]; !ok {
			letters = append(letters, letter)
		}
		byLetter[letter] = append(byLetter[letter], a)
	}
	sort.Strings(letters)
	idx := make([]Index, 0, len(letters))
	for _, l := range letters {
		idx = append(idx, Index{Name: l, Artists: byLetter[l]})
	}
	return idx
}

func indexLetter(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "#"
	}
	c := strings.ToUpper(name[:1])
	if c >= "A" && c <= "Z" {
		return c
	}
	return "#"
}

func (s *Server) GetMusicDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	cat := s.catalog()
	dir := &Directory{ID: id}

	// Root: one child per cached artist.
	if id == "" || id == "0" || id == "-1" {
		dir.Name = "Music"
		for _, artist := range cat.artists {
			dir.Children = append(dir.Children, DirectoryChild{
				ID: metadata.EncodeID(artist, "", ""), IsDir: true,
				Title: artist, Artist: artist,
				ArtistID: metadata.EncodeID(artist, "", ""), Type: "music",
			})
		}
		resp := okResponse()
		resp.MusicDirectory = dir
		write(w, r, resp)
		return
	}

	artist, album, title, err := metadata.SplitID(id)
	if err != nil {
		writeFail(w, r, 70, "Directory not found")
		return
	}
	switch {
	case title != "": // a song has no children
		writeFail(w, r, 70, "Directory not found")
		return
	case album != "": // album -> its songs
		dir.Name = displayAlbum(album)
		dir.Parent = metadata.EncodeID(artist, "", "")
		for _, cs := range cat.byArtist[artist][album] {
			dir.Children = append(dir.Children, dirChild(cat.entry(cs)))
		}
	default: // artist -> its albums
		dir.Name = artist
		dir.Parent = "0"
		for _, al := range cat.sortedAlbums(artist) {
			e := cat.albumEntry(artist, al)
			dir.Children = append(dir.Children, DirectoryChild{
				ID: e.ID, Parent: id, IsDir: true, Title: e.Name,
				Artist: artist, CoverArt: e.CoverArt, AlbumID: e.ID,
				ArtistID: e.ArtistID, Type: "music",
			})
		}
	}
	resp := okResponse()
	resp.MusicDirectory = dir
	write(w, r, resp)
}

func dirChild(e SongEntry) DirectoryChild {
	return DirectoryChild{
		ID: e.ID, Parent: e.Parent, IsDir: false, Title: e.Title,
		Album: e.Album, Artist: e.Artist, Track: e.Track,
		CoverArt: e.CoverArt, Duration: e.Duration, Suffix: e.Suffix,
		ContentType: e.ContentType, Size: e.Size, AlbumID: e.AlbumID,
		ArtistID: e.ArtistID, Type: e.Type,
	}
}

// --- search2 / genres / similar / top ------------------------------------

func (s *Server) Search2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	res, err := s.searchAll(r)
	if err != nil {
		writeFail(w, r, 0, "search failed: "+err.Error())
		return
	}
	resp := okResponse()
	resp.SearchResult2 = &SearchResult2{
		Songs: res.Songs, Albums: res.Albums, Artists: res.Artists,
	}
	s.prewarmPage(res.Songs)
	write(w, r, resp)
}

func (s *Server) Genres(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	// No genre metadata is stored: return a valid empty list so clients render
	// an empty state instead of failing.
	resp := okResponse()
	resp.Genres = &Genres{}
	write(w, r, resp)
}

func (s *Server) SongsByGenre(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.SongsByGenre = &Songs{}
	write(w, r, resp)
}

func (s *Server) TopSongs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.TopSongs = &TopSongs{}
	write(w, r, resp)
}

func (s *Server) SimilarSongs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.SimilarSongs = &SimilarSongs{}
	write(w, r, resp)
}

func (s *Server) SimilarSongs2(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.SimilarSongs2 = &SimilarSongs2{}
	write(w, r, resp)
}

// --- download / users / avatar / misc stubs -------------------------------

func (s *Server) Download(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	_ = r.ParseForm()
	id := firstOf(r.Form, "id")
	artist, _, title, err := metadata.DecodeID(id)
	if err != nil {
		writeFail(w, r, 70, "Song not found")
		return
	}
	if path, ok := s.storage.Lookup(id); ok {
		name := strings.Map(safeFilenameRune, artist+" - "+title)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		s.serveCached(w, r, path)
		return
	}
	// Not cached: resolve through the normal pipeline (seeks/upgrades apply).
	s.Stream(w, r)
}

func safeFilenameRune(r rune) rune {
	switch r {
	case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
		return '-'
	}
	return r
}

// userEntry is the single configured user, advertised with full playback and
// playlist rights so clients unlock those UIs.
func (s *Server) userEntry() User {
	return User{
		Username:          s.user,
		ScrobblingEnabled: true,
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
}

func (s *Server) GetUsers(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Users = &Users{Users: []User{s.userEntry()}}
	write(w, r, resp)
}

func (s *Server) GetAvatar(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(transparentPNG)
}

// 1x1 transparent PNG.
var transparentPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func (s *Server) EmptyShares(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Shares = &Shares{}
	write(w, r, resp)
}

func (s *Server) EmptyBookmarks(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Bookmarks = &Bookmarks{}
	write(w, r, resp)
}

func (s *Server) EmptyRadioStations(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.RadioStations = &InternetRadioStations{}
	write(w, r, resp)
}

func (s *Server) EmptyVideos(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Videos = &Videos{}
	write(w, r, resp)
}

func (s *Server) EmptyPodcasts(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.Podcasts = &Podcasts{}
	write(w, r, resp)
}

func (s *Server) EmptyChat(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	resp := okResponse()
	resp.ChatMessages = &ChatMessages{}
	write(w, r, resp)
}

// NotImplemented answers endpoints for features this server has no backing
// store for. Returning a valid Subsonic error is better than a bare 404: the
// client shows "not supported" instead of treating the server as broken.
func (s *Server) NotImplemented(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	writeFail(w, r, 0, "Not supported by MusicStreamer")
}

// --- helpers --------------------------------------------------------------

func boolParam(form map[string][]string, key string, def bool) bool {
	v, ok := formValue(form, key)
	if !ok || v == "" {
		return def
	}
	return boolValue(v, def)
}

func boolValue(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

// formValue returns the first non-empty value for key.
func formValue(form map[string][]string, key string) (string, bool) {
	if vs := form[key]; len(vs) > 0 {
		return vs[0], true
	}
	return "", false
}

func intParams(vals []string) []int {
	var out []int
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}
