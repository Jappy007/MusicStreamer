package subsonic

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/metadata"
)

const nativeTokenTTL = 7 * 24 * time.Hour

type nativeSession struct {
	user string
	exp  time.Time
}

type nativeLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type nativeLoginResponse struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Name          string `json:"name"`
	IsAdmin       bool   `json:"isAdmin"`
	Token         string `json:"token"`
	UserID        string `json:"userId"`
	SubsonicSalt  string `json:"subsonicSalt"`
	SubsonicToken string `json:"subsonicToken"`
}

func (s *Server) NativeLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeNativeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req nativeLoginRequest
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		_ = json.Unmarshal(body, &req)
	} else {
		_ = r.ParseForm()
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
	}
	if !secureEqual(req.Username, s.user) || !secureEqual(req.Password, s.pass) {
		writeNativeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := newNativeToken()
	if err != nil {
		writeNativeError(w, http.StatusInternalServerError, "token generation failed")
		return
	}
	id := nativeUserID(s.user)
	salt := newNativeSalt()
	subToken := subsonicToken(s.pass, salt)
	s.nativeMu.Lock()
	if s.nativeTokens == nil {
		s.nativeTokens = make(map[string]nativeSession)
	}
	now := time.Now()
	for k, sess := range s.nativeTokens {
		if now.After(sess.exp) {
			delete(s.nativeTokens, k)
		}
	}
	s.nativeTokens[token] = nativeSession{user: s.user, exp: now.Add(nativeTokenTTL)}
	s.nativeMu.Unlock()
	writeNativeJSON(w, http.StatusOK, nativeLoginResponse{
		ID: id, Username: s.user, Name: s.user, IsAdmin: true,
		Token: token, UserID: id, SubsonicSalt: salt, SubsonicToken: subToken,
	})
}

func newNativeSalt() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0123456789abcdef"
	}
	return hex.EncodeToString(b[:])
}

func subsonicToken(password, salt string) string {
	sum := md5.Sum([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

func newNativeToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func nativeUserID(user string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(user))))
	return hex.EncodeToString(sum[:16])
}

func secureEqual(got, want string) bool {
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *Server) nativeAuthed(w http.ResponseWriter, r *http.Request) bool {
	token := ""
	if v := r.Header.Get("X-ND-Authorization"); v != "" {
		token = v
	} else if v := r.Header.Get("Authorization"); v != "" {
		token = v
	} else if c, err := r.Cookie("NDToken"); err == nil {
		token = c.Value
	}
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		writeNativeError(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	s.nativeMu.Lock()
	sess, ok := s.nativeTokens[token]
	if ok && time.Now().After(sess.exp) {
		delete(s.nativeTokens, token)
		ok = false
	}
	s.nativeMu.Unlock()
	if !ok {
		writeNativeError(w, http.StatusUnauthorized, "invalid or expired token")
		return false
	}
	return true
}

func (s *Server) NativeKeepalive(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	writeNativeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- native query parsing -------------------------------------------------

type nativeQuery struct {
	filters map[string][]string
	sort    string
	order   string
	start   int
	end     int
}

func parseNativeQuery(r *http.Request) nativeQuery {
	q := nativeQuery{filters: make(map[string][]string), start: -1, end: -1}
	values := r.URL.Query()
	for k, vals := range values {
		switch k {
		case "_sort":
			if len(vals) > 0 {
				q.sort = vals[0]
			}
			continue
		case "_order":
			if len(vals) > 0 {
				q.order = vals[0]
			}
			continue
		case "_start":
			q.start = atoiDefault(vals, 0)
			continue
		case "_end":
			q.end = atoiDefault(vals, -1)
			continue
		case "_filters":
			if len(vals) > 0 {
				q.addJSONFilters(vals[0])
			}
			continue
		}
		for _, v := range vals {
			if v != "" {
				q.filters[k] = append(q.filters[k], v)
			}
		}
	}
	return q
}

func atoiDefault(vals []string, def int) int {
	if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(vals[0]))
	if err != nil {
		return def
	}
	return n
}

func (q *nativeQuery) addJSONFilters(raw string) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for k, v := range obj {
			q.addFilterValue(k, v)
		}
		return
	}
	var arr []struct {
		Field    string `json:"field"`
		Operator string `json:"operator"`
		Value    any    `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		for _, f := range arr {
			q.addFilterValue(f.Field, f.Value)
		}
	}
}

func (q *nativeQuery) addFilterValue(key string, value any) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	switch v := value.(type) {
	case nil:
		return
	case []any:
		for _, item := range v {
			q.addFilterValue(key, item)
		}
	case string:
		if v != "" {
			q.filters[key] = append(q.filters[key], v)
		}
	case bool:
		q.filters[key] = append(q.filters[key], strconv.FormatBool(v))
	case float64:
		q.filters[key] = append(q.filters[key], strconv.FormatFloat(v, 'f', -1, 64))
	default:
		q.filters[key] = append(q.filters[key], fmt.Sprint(v))
	}
}

func (q nativeQuery) values(key string) []string {
	var out []string
	for _, v := range q.filters[key] {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (q nativeQuery) matchID(key string, got ...string) bool {
	vals := q.values(key)
	if len(vals) == 0 {
		return true
	}
	for _, v := range vals {
		for _, g := range got {
			if strings.EqualFold(v, g) {
				return true
			}
		}
	}
	return false
}

func (q nativeQuery) contains(key string, got ...string) bool {
	vals := q.values(key)
	if len(vals) == 0 {
		return true
	}
	for _, v := range vals {
		lv := strings.ToLower(v)
		for _, g := range got {
			if strings.Contains(strings.ToLower(g), lv) {
				return true
			}
		}
	}
	return false
}

func (q nativeQuery) boolFilter(key string) (want bool, ok bool) {
	vals := q.values(key)
	if len(vals) == 0 {
		return false, false
	}
	v, err := strconv.ParseBool(vals[0])
	if err != nil {
		return false, false
	}
	return v, true
}

func (q nativeQuery) intFilter(key string) (int, bool) {
	vals := q.values(key)
	if len(vals) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(vals[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (q nativeQuery) hasRoleFilter() bool {
	for k, vals := range q.filters {
		if !strings.HasPrefix(k, "role") {
			continue
		}
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

func (q nativeQuery) sortSpec() (field string, desc bool) {
	raw := strings.TrimSpace(q.sort)
	if raw == "" {
		return "", strings.EqualFold(q.order, "DESC")
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if json.Unmarshal([]byte(raw), &arr) == nil && len(arr) > 0 {
			raw = arr[0]
			if len(arr) > 1 {
				q.order = arr[1]
			}
		}
	}
	parts := strings.Split(raw, ",")
	field = strings.TrimSpace(parts[0])
	if f := strings.Fields(field); len(f) > 0 {
		field = f[0]
		if len(f) > 1 {
			q.order = f[1]
		}
	}
	if len(parts) > 1 {
		if order := strings.TrimSpace(parts[1]); order != "" {
			q.order = order
		}
	}
	desc = strings.EqualFold(q.order, "DESC")
	if strings.HasPrefix(field, "-") {
		field = strings.TrimPrefix(field, "-")
		desc = !desc
	}
	return field, desc
}

func (q nativeQuery) bounds(n int) (int, int) {
	if n <= 0 {
		return 0, 0
	}
	start := q.start
	if start < 0 || start > n {
		start = 0
	}
	end := q.end
	if end <= 0 || end > n {
		end = n
	}
	if end < start {
		end = start
	}
	return start, end
}

// --- native song endpoints ------------------------------------------------

type nativeSong struct {
	ID            string  `json:"id"`
	MediaFileID   string  `json:"mediaFileId"`
	LibraryID     int     `json:"libraryId"`
	Title         string  `json:"title"`
	Album         string  `json:"album"`
	AlbumID       string  `json:"albumId"`
	Artist        string  `json:"artist"`
	ArtistID      string  `json:"artistId"`
	AlbumArtist   string  `json:"albumArtist"`
	AlbumArtistID string  `json:"albumArtistId"`
	TrackNumber   int     `json:"trackNumber"`
	DiscNumber    int     `json:"discNumber"`
	Year          int     `json:"year"`
	Genre         string  `json:"genre"`
	Duration      float64 `json:"duration"`
	Size          int64   `json:"size"`
	BitRate       int     `json:"bitRate"`
	Suffix        string  `json:"suffix"`
	ContentType   string  `json:"contentType"`
	Path          string  `json:"path"`
	CoverArt      string  `json:"coverArt"`
	CoverArtID    string  `json:"coverArtId"`
	PlayCount     int     `json:"playCount"`
	PlayDate      string  `json:"playDate"`
	Rating        int     `json:"rating"`
	Starred       bool    `json:"starred"`
	StarredAt     *string `json:"starredAt"`
	SampleRate    int     `json:"sampleRate"`
	BitDepth      int     `json:"bitDepth"`
	ChannelCount  int     `json:"channelCount"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

func (s *Server) nativeSongs(q nativeQuery) []nativeSong {
	cat := s.catalog()
	starred := s.starredSongSet()
	out := make([]nativeSong, 0, len(cat.songs))
	for _, cs := range cat.songs {
		e := s.cachedEntry(cs)
		if n, ok := q.intFilter("year"); ok && e.Year != n {
			continue
		}
		if n, ok := q.intFilter("min_year"); ok && e.Year != 0 && e.Year < n {
			continue
		}
		if n, ok := q.intFilter("max_year"); ok && e.Year != 0 && e.Year > n {
			continue
		}
		if !q.matchID("album_id", e.AlbumID) || !q.matchID("artist_id", e.ArtistID) {
			continue
		}
		if !q.contains("title", e.Title) || !q.contains("name", e.Title) {
			continue
		}
		if !q.contains("artist", e.Artist) || !q.contains("album", e.Album) {
			continue
		}
		if want, ok := q.boolFilter("starred"); ok && starred[e.ID] != want {
			continue
		}
		if n, ok := q.intFilter("rating"); ok && e.UserRating != n {
			continue
		}
		if want, ok := q.boolFilter("missing"); ok && want {
			continue
		}
		if vals := q.values("library_id"); len(vals) > 0 && !containsValue(vals, "0") {
			continue
		}
		if !q.matchID("id", e.ID) {
			continue
		}
		out = append(out, nativeSongFromEntry(e, starred[e.ID]))
	}
	sortNativeSongs(out, q)
	return out
}

func nativeSongFromEntry(e SongEntry, starred bool) nativeSong {
	starredAt := (*string)(nil)
	if starred {
		t := "1970-01-01T00:00:00Z"
		starredAt = &t
	}
	cover := e.CoverArt
	return nativeSong{
		ID: e.ID, MediaFileID: e.ID, LibraryID: 0,
		Title: e.Title, Album: e.Album, AlbumID: e.AlbumID,
		Artist: e.Artist, ArtistID: e.ArtistID,
		AlbumArtist: e.Artist, AlbumArtistID: e.ArtistID,
		TrackNumber: e.Track, Year: e.Year, Genre: e.Genre,
		Duration: float64(e.Duration), Size: e.Size, BitRate: e.BitRate,
		Suffix: e.Suffix, ContentType: e.ContentType, Path: e.Path,
		CoverArt: cover, CoverArtID: cover,
		PlayCount: e.PlayCount, PlayDate: "", Rating: e.UserRating,
		Starred: starred, StarredAt: starredAt,
		SampleRate: e.SamplingRate, BitDepth: e.BitDepth, ChannelCount: e.ChannelCount,
	}
}

func sortNativeSongs(songs []nativeSong, q nativeQuery) {
	field, desc := q.sortSpec()
	if field == "" {
		field = "title"
	}
	if field == "random" {
		mathrand.Shuffle(len(songs), func(i, j int) { songs[i], songs[j] = songs[j], songs[i] })
		return
	}
	less := func(a, b nativeSong) bool {
		switch field {
		case "album":
			if !strings.EqualFold(a.Album, b.Album) {
				return strings.ToLower(a.Album) < strings.ToLower(b.Album)
			}
			return a.TrackNumber < b.TrackNumber
		case "artist", "albumArtist":
			if !strings.EqualFold(a.Artist, b.Artist) {
				return strings.ToLower(a.Artist) < strings.ToLower(b.Artist)
			}
			return strings.ToLower(a.Album) < strings.ToLower(b.Album)
		case "duration":
			return a.Duration < b.Duration
		case "size":
			return a.Size < b.Size
		case "year", "min_year", "max_year":
			return a.Year < b.Year
		case "trackNumber", "track":
			return a.TrackNumber < b.TrackNumber
		case "playCount", "play_count", "playDate", "play_date":
			return a.PlayCount < b.PlayCount
		case "rating":
			return a.Rating < b.Rating
		case "genre":
			return strings.ToLower(a.Genre) < strings.ToLower(b.Genre)
		case "recently_added", "createdAt", "created_at":
			return a.ID < b.ID
		default:
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		}
	}
	sort.SliceStable(songs, func(i, j int) bool {
		if desc {
			return less(songs[j], songs[i])
		}
		return less(songs[i], songs[j])
	})
}

func (s *Server) NativeSongs(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeNativeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := parseNativeQuery(r)
	songs := s.nativeSongs(q)
	start, end := q.bounds(len(songs))
	setNativeTotal(w, len(songs), start, end)
	writeNativeJSON(w, http.StatusOK, songs[start:end])
}

func (s *Server) NativeSong(w http.ResponseWriter, r *http.Request, id string) {
	if !s.nativeAuthed(w, r) {
		return
	}
	e, ok := s.catalog().songForID(id)
	if !ok {
		writeNativeError(w, http.StatusNotFound, "song not found")
		return
	}
	starred := s.starredSongSet()
	writeNativeJSON(w, http.StatusOK, nativeSongFromEntry(e, starred[id]))
}

// --- native album endpoints -----------------------------------------------

type nativeAlbum struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Artist               string   `json:"artist"`
	ArtistID             string   `json:"artistId"`
	AlbumArtist          string   `json:"albumArtist"`
	AlbumArtistID        string   `json:"albumArtistId"`
	AllArtistIDs         string   `json:"allArtistIds"`
	CoverArt             string   `json:"coverArt"`
	CoverArtID           string   `json:"coverArtId"`
	SongCount            int      `json:"songCount"`
	Duration             float64  `json:"duration"`
	Size                 int64    `json:"size"`
	PlayCount            int      `json:"playCount"`
	PlayDate             string   `json:"playDate"`
	Rating               int      `json:"rating"`
	Starred              bool     `json:"starred"`
	StarredAt            *string  `json:"starredAt"`
	Genre                string   `json:"genre"`
	Genres               []string `json:"genres"`
	MinYear              int      `json:"minYear"`
	MaxYear              int      `json:"maxYear"`
	Date                 string   `json:"date"`
	Releases             int      `json:"releases"`
	Compilation          bool     `json:"compilation"`
	FullText             string   `json:"fullText"`
	OrderAlbumName       string   `json:"orderAlbumName"`
	OrderAlbumArtistName string   `json:"orderAlbumArtistName"`
	CreatedAt            string   `json:"createdAt"`
	UpdatedAt            string   `json:"updatedAt"`
}

func (s *Server) nativeAlbums(q nativeQuery) []nativeAlbum {
	if q.hasRoleFilter() {
		return nil
	}
	cat := s.catalog()
	starred := s.starredAlbumSet()
	var out []nativeAlbum
	for _, artist := range cat.artists {
		for _, album := range cat.sortedAlbums(artist) {
			aid := metadata.EncodeID(artist, album, "")
			if !q.matchID("artist_id", metadata.EncodeID(artist, "", "")) ||
				!q.contains("name", displayAlbum(album)) ||
				!q.contains("albumArtist", artist) ||
				!q.matchID("id", aid) {
				continue
			}
			if want, ok := q.boolFilter("starred"); ok && starred[aid] != want {
				continue
			}
			if want, ok := q.boolFilter("compilation"); ok && want {
				continue
			}
			if vals := q.values("library_id"); len(vals) > 0 && !containsValue(vals, "0") {
				continue
			}
			a := s.nativeAlbumFor(artist, album, cat.byArtist[artist][album], starred[aid])
			if n, ok := q.intFilter("year"); ok && a.MinYear != n {
				continue
			}
			if n, ok := q.intFilter("rating"); ok && a.Rating != n {
				continue
			}
			if !q.contains("genre", a.Genre) {
				continue
			}
			out = append(out, a)
		}
	}
	sortNativeAlbums(out, q)
	return out
}

func (s *Server) nativeAlbumFor(artist, album string, songs []cachedSong, starred bool) nativeAlbum {
	aid := metadata.EncodeID(artist, album, "")
	a := nativeAlbum{
		ID: aid, Name: displayAlbum(album), Artist: artist,
		ArtistID:    metadata.EncodeID(artist, "", ""),
		AlbumArtist: artist, AlbumArtistID: metadata.EncodeID(artist, "", ""),
		AllArtistIDs: metadata.EncodeID(artist, "", ""),
		CoverArt:     aid, CoverArtID: aid,
		SongCount: len(songs), Genres: []string{}, Releases: 1,
		FullText:             " " + displayAlbum(album) + " " + artist,
		OrderAlbumName:       strings.ToLower(displayAlbum(album)),
		OrderAlbumArtistName: strings.ToLower(artist),
	}
	for _, cs := range songs {
		e := s.cachedEntry(cs)
		a.Duration += float64(e.Duration)
		a.Size += e.Size
		a.PlayCount += e.PlayCount
		if a.Genre == "" {
			a.Genre = e.Genre
		}
		if a.MinYear == 0 || (e.Year != 0 && e.Year < a.MinYear) {
			a.MinYear = e.Year
		}
		if e.Year > a.MaxYear {
			a.MaxYear = e.Year
		}
	}
	if a.MinYear == 0 && a.MaxYear != 0 {
		a.MinYear = a.MaxYear
	}
	a.Date = strconv.Itoa(a.MinYear)
	if a.Genre != "" {
		a.Genres = []string{a.Genre}
	}
	if s.lib != nil {
		a.Rating = s.lib.Rating(aid)
	}
	a.Starred = starred
	if starred {
		t := "1970-01-01T00:00:00Z"
		a.StarredAt = &t
	}
	return a
}

func sortNativeAlbums(albums []nativeAlbum, q nativeQuery) {
	field, desc := q.sortSpec()
	if field == "" {
		field = "name"
	}
	if field == "random" {
		mathrand.Shuffle(len(albums), func(i, j int) { albums[i], albums[j] = albums[j], albums[i] })
		return
	}
	less := func(a, b nativeAlbum) bool {
		switch field {
		case "artist", "albumArtist":
			if !strings.EqualFold(a.Artist, b.Artist) {
				return strings.ToLower(a.Artist) < strings.ToLower(b.Artist)
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		case "min_year", "year", "date":
			return a.MinYear < b.MinYear
		case "max_year":
			return a.MaxYear < b.MaxYear
		case "play_count", "playDate", "play_date":
			return a.PlayCount < b.PlayCount
		case "songCount", "song_count":
			return a.SongCount < b.SongCount
		case "duration":
			return a.Duration < b.Duration
		case "size":
			return a.Size < b.Size
		case "rating":
			return a.Rating < b.Rating
		case "createdAt", "created_at":
			return a.ID < b.ID
		default:
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
	}
	sort.SliceStable(albums, func(i, j int) bool {
		if desc {
			return less(albums[j], albums[i])
		}
		return less(albums[i], albums[j])
	})
}

func (s *Server) NativeAlbums(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeNativeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := parseNativeQuery(r)
	albums := s.nativeAlbums(q)
	if albums == nil {
		albums = []nativeAlbum{}
	}
	start, end := q.bounds(len(albums))
	setNativeTotal(w, len(albums), start, end)
	writeNativeJSON(w, http.StatusOK, albums[start:end])
}

func (s *Server) NativeAlbum(w http.ResponseWriter, r *http.Request, id string) {
	if !s.nativeAuthed(w, r) {
		return
	}
	artist, album, _, err := metadata.SplitID(id)
	if err != nil || album == "" {
		writeNativeError(w, http.StatusNotFound, "album not found")
		return
	}
	songs := s.catalog().byArtist[artist][album]
	if len(songs) == 0 {
		writeNativeError(w, http.StatusNotFound, "album not found")
		return
	}
	starred := s.starredAlbumSet()
	writeNativeJSON(w, http.StatusOK, s.nativeAlbumFor(artist, album, songs, starred[id]))
}

// --- native artist endpoints ----------------------------------------------

type nativeArtist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CoverArt   string  `json:"coverArt"`
	CoverArtID string  `json:"coverArtId"`
	AlbumCount int     `json:"albumCount"`
	SongCount  int     `json:"songCount"`
	Size       int64   `json:"size"`
	Rating     int     `json:"rating"`
	Starred    bool    `json:"starred"`
	StarredAt  *string `json:"starredAt"`
}

func (s *Server) nativeArtists(q nativeQuery) []nativeArtist {
	if q.hasRoleFilter() {
		return nil
	}
	cat := s.catalog()
	starred := s.starredArtistSet()
	out := make([]nativeArtist, 0, len(cat.artists))
	for _, artist := range cat.artists {
		id := metadata.EncodeID(artist, "", "")
		if !q.contains("name", artist) || !q.matchID("id", id) {
			continue
		}
		if want, ok := q.boolFilter("starred"); ok && starred[id] != want {
			continue
		}
		if vals := q.values("library_id"); len(vals) > 0 && !containsValue(vals, "0") {
			continue
		}
		a := nativeArtist{ID: id, Name: artist, CoverArt: id, CoverArtID: id,
			AlbumCount: len(cat.byArtist[artist]), Starred: starred[id]}
		for _, album := range cat.byArtist[artist] {
			a.SongCount += len(album)
			for _, cs := range album {
				a.Size += cs.size
			}
		}
		if a.Starred {
			t := "1970-01-01T00:00:00Z"
			a.StarredAt = &t
		}
		out = append(out, a)
	}
	sortNativeArtists(out, q)
	return out
}

func sortNativeArtists(artists []nativeArtist, q nativeQuery) {
	field, desc := q.sortSpec()
	if field == "" {
		field = "name"
	}
	if field == "random" {
		mathrand.Shuffle(len(artists), func(i, j int) { artists[i], artists[j] = artists[j], artists[i] })
		return
	}
	less := func(a, b nativeArtist) bool {
		switch field {
		case "albumCount", "album_count":
			return a.AlbumCount < b.AlbumCount
		case "songCount", "song_count":
			return a.SongCount < b.SongCount
		case "size":
			return a.Size < b.Size
		case "play_count", "play_date":
			return a.Name < b.Name
		default:
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
	}
	sort.SliceStable(artists, func(i, j int) bool {
		if desc {
			return less(artists[j], artists[i])
		}
		return less(artists[i], artists[j])
	})
}

func (s *Server) NativeArtists(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeNativeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := parseNativeQuery(r)
	artists := s.nativeArtists(q)
	if artists == nil {
		artists = []nativeArtist{}
	}
	start, end := q.bounds(len(artists))
	setNativeTotal(w, len(artists), start, end)
	writeNativeJSON(w, http.StatusOK, artists[start:end])
}

func (s *Server) NativeArtist(w http.ResponseWriter, r *http.Request, id string) {
	if !s.nativeAuthed(w, r) {
		return
	}
	e, ok := s.artistEntryForID(id)
	if !ok {
		writeNativeError(w, http.StatusNotFound, "artist not found")
		return
	}
	cat := s.catalog()
	starred := s.starredArtistSet()
	a := nativeArtist{ID: e.ID, Name: e.Name, CoverArt: e.ID, CoverArtID: e.ID,
		AlbumCount: e.AlbumCount, Starred: starred[e.ID]}
	for _, album := range cat.byArtist[e.Name] {
		a.SongCount += len(album)
		for _, cs := range album {
			a.Size += cs.size
		}
	}
	if a.Starred {
		t := "1970-01-01T00:00:00Z"
		a.StarredAt = &t
	}
	writeNativeJSON(w, http.StatusOK, a)
}

// --- native library + images ----------------------------------------------

type nativeLibrary struct {
	ID                 int    `json:"id"`
	Name               string `json:"name"`
	Path               string `json:"path"`
	RemotePath         string `json:"remotePath"`
	LastScanAt         string `json:"lastScanAt"`
	LastScanStartedAt  string `json:"lastScanStartedAt"`
	LastScanDuration   int64  `json:"lastScanDuration"`
	LastScanError      string `json:"lastScanError"`
	FullScanInProgress bool   `json:"fullScanInProgress"`
	TotalSongs         int    `json:"totalSongs"`
	CreatedAt          string `json:"createdAt"`
	UpdatedAt          string `json:"updatedAt"`
}

func (s *Server) NativeLibraries(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	cat := s.catalog()
	writeNativeJSON(w, http.StatusOK, []nativeLibrary{{
		ID: 0, Name: "Music", TotalSongs: len(cat.songs),
	}})
}

func (s *Server) NativeImage(w http.ResponseWriter, r *http.Request, id string) {
	if !s.nativeAuthed(w, r) {
		return
	}
	s.serveCover(w, r, id)
}

func (s *Server) NativeMissingImage(w http.ResponseWriter, r *http.Request) {
	if !s.nativeAuthed(w, r) {
		return
	}
	writeNativeError(w, http.StatusNotFound, "image not found")
}

func (s *Server) NativeSongSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/song/")
	if strings.HasSuffix(rest, "/image") || strings.HasSuffix(rest, "/cover") {
		id := strings.TrimSuffix(strings.TrimSuffix(rest, "/image"), "/cover")
		s.nativeImageSub(w, r, id)
		return
	}
	id, err := url.PathUnescape(strings.Trim(rest, "/"))
	if err != nil || id == "" {
		writeNativeError(w, http.StatusNotFound, "song not found")
		return
	}
	s.NativeSong(w, r, id)
}

func (s *Server) NativeAlbumSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/album/")
	if strings.HasSuffix(rest, "/image") || strings.HasSuffix(rest, "/cover") {
		id := strings.TrimSuffix(strings.TrimSuffix(rest, "/image"), "/cover")
		s.nativeImageSub(w, r, id)
		return
	}
	id, err := url.PathUnescape(strings.Trim(rest, "/"))
	if err != nil || id == "" {
		writeNativeError(w, http.StatusNotFound, "album not found")
		return
	}
	s.NativeAlbum(w, r, id)
}

func (s *Server) NativeArtistSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/artist/")
	if strings.HasSuffix(rest, "/image") || strings.HasSuffix(rest, "/cover") {
		id := strings.TrimSuffix(strings.TrimSuffix(rest, "/image"), "/cover")
		s.nativeImageSub(w, r, id)
		return
	}
	id, err := url.PathUnescape(strings.Trim(rest, "/"))
	if err != nil || id == "" {
		writeNativeError(w, http.StatusNotFound, "artist not found")
		return
	}
	s.NativeArtist(w, r, id)
}

func (s *Server) nativeImageSub(w http.ResponseWriter, r *http.Request, id string) {
	id, err := url.PathUnescape(strings.Trim(id, "/"))
	if err != nil || id == "" {
		writeNativeError(w, http.StatusNotFound, "image not found")
		return
	}
	s.NativeImage(w, r, id)
}

// --- helpers --------------------------------------------------------------

func stringSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func (s *Server) starredSongSet() map[string]bool {
	if s.lib == nil {
		return nil
	}
	return stringSet(s.lib.StarredSongs())
}

func (s *Server) starredAlbumSet() map[string]bool {
	if s.lib == nil {
		return nil
	}
	return stringSet(s.lib.StarredAlbums())
}

func (s *Server) starredArtistSet() map[string]bool {
	if s.lib == nil {
		return nil
	}
	return stringSet(s.lib.StarredArtists())
}

func containsValue(items []string, want string) bool {
	for _, item := range items {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

func setNativeTotal(w http.ResponseWriter, total, start, end int) {
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	if total == 0 {
		w.Header().Set("Content-Range", "items */0")
		return
	}
	if end > start {
		w.Header().Set("Content-Range", fmt.Sprintf("items %d-%d/%d", start, end-1, total))
	} else {
		w.Header().Set("Content-Range", fmt.Sprintf("items */%d", total))
	}
}

func writeNativeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeNativeError(w http.ResponseWriter, status int, msg string) {
	writeNativeJSON(w, status, map[string]string{"error": msg})
}
