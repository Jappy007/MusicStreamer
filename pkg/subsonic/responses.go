package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
)

// Protocol versions.
const (
	restVersion   = "1.16.1"
	serverVersion = "1.0.0"
	serverType    = "DebridSubsonic"
)

// Response is the top-level Subsonic envelope. It serializes to both the
// JSON shape {"subsonic-response": {...}} and the XML shape
// <subsonic-response ...>...</subsonic-response>.
type Response struct {
	XMLName       xml.Name `xml:"subsonic-response" json:"-"`
	Status        string   `xml:"status,attr" json:"status"`
	Version       string   `xml:"version,attr" json:"version"`
	Type          string   `xml:"type,attr" json:"type"`
	ServerVersion string   `xml:"serverVersion,attr" json:"serverVersion"`
	OpenSubsonic  bool     `xml:"openSubsonic,attr" json:"openSubsonic"`

	Error         *RespError    `xml:"error,omitempty" json:"error,omitempty"`
	License       *License      `xml:"license,omitempty" json:"license,omitempty"`
	User          *User         `xml:"user,omitempty" json:"user,omitempty"`
	SearchResult3 *SearchResult `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	Song          *SongEntry    `xml:"song,omitempty" json:"song,omitempty"`

	Playlists  *Playlists     `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Genres     *Genres        `xml:"genres,omitempty" json:"genres,omitempty"`
	AlbumList  *AlbumList     `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2 *AlbumList2    `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	Album      *AlbumInfo     `xml:"album,omitempty" json:"album,omitempty"`
	Starred    *Starred       `xml:"starred,omitempty" json:"starred,omitempty"`
	Starred2   *Starred2      `xml:"starred2,omitempty" json:"starred2,omitempty"`
	Jukebox    *JukeboxStatus `xml:"jukeboxStatus,omitempty" json:"jukeboxStatus,omitempty"`
	Artist     *ArtistInfo    `xml:"artist,omitempty" json:"artist,omitempty"`

	// Minimal compat stubs so library-browsing clients don't error out.
	MusicFolders *MusicFolders `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	Artists      *Artists      `xml:"artists,omitempty" json:"artists,omitempty"`
	Extensions   *Extensions   `xml:"openSubsonicExtensions,omitempty" json:"openSubsonicExtensions,omitempty"`
	ServerInfo   *ServerInfo   `xml:"serverInfo,omitempty" json:"serverInfo,omitempty"`
}

type RespError struct {
	XMLName xml.Name `xml:"error" json:"-"`
	Code    int      `xml:"code,attr" json:"code"`
	Message string   `xml:"message,attr" json:"message"`
}

type License struct {
	XMLName     xml.Name `xml:"license" json:"-"`
	Valid       bool     `xml:"valid,attr" json:"valid"`
	Email       string   `xml:"email,attr,omitempty" json:"email,omitempty"`
	Key         string   `xml:"key,attr,omitempty" json:"key,omitempty"`
	Date        string   `xml:"date,attr,omitempty" json:"date,omitempty"`
	Description string   `xml:",chardata" json:"-"`
}

// User is the getUser.view payload. Roles advertise full streaming +
// playlist rights so Feishin unlocks playback and playlist UIs.
type User struct {
	XMLName             xml.Name `xml:"user" json:"-"`
	Username            string   `xml:"username,attr" json:"username"`
	Email               string   `xml:"email,attr,omitempty" json:"email,omitempty"`
	ScrobblingEnabled   bool     `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	AdminRole           bool     `xml:"adminRole,attr" json:"adminRole"`
	SettingsRole        bool     `xml:"settingsRole,attr" json:"settingsRole"`
	DownloadRole        bool     `xml:"downloadRole,attr" json:"downloadRole"`
	UploadRole          bool     `xml:"uploadRole,attr" json:"uploadRole"`
	PlaylistRole        bool     `xml:"playlistRole,attr" json:"playlistRole"`
	CoverArtRole        bool     `xml:"coverArtRole,attr" json:"coverArtRole"`
	CommentRole         bool     `xml:"commentRole,attr" json:"commentRole"`
	PodcastRole         bool     `xml:"podcastRole,attr" json:"podcastRole"`
	StreamRole          bool     `xml:"streamRole,attr" json:"streamRole"`
	JukeboxRole         bool     `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	ShareRole           bool     `xml:"shareRole,attr" json:"shareRole"`
	VideoConversionRole bool     `xml:"videoConversionRole,attr" json:"videoConversionRole"`
	Folders             []int    `xml:"folder" json:"folder"`
}

// Empty-library stubs: valid envelopes with zero entries, so clients
// render empty states instead of errors.
type Playlists struct {
	XMLName xml.Name   `xml:"playlists" json:"-"`
	Items   []Playlist `xml:"playlist" json:"playlist,omitempty"`
}

type Playlist struct {
	XMLName   xml.Name `xml:"playlist" json:"-"`
	ID        string   `xml:"id,attr" json:"id"`
	Name      string   `xml:"name,attr" json:"name"`
	SongCount int      `xml:"songCount,attr" json:"songCount"`
	Duration  int      `xml:"duration,attr" json:"duration"`
	Public    bool     `xml:"public,attr" json:"public"`
	Owner     string   `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	Created   string   `xml:"created,attr,omitempty" json:"created,omitempty"`
}

type Genres struct {
	XMLName xml.Name `xml:"genres" json:"-"`
	Items   []string `xml:"genre" json:"genre,omitempty"`
}

type AlbumList struct {
	XMLName xml.Name     `xml:"albumList" json:"-"`
	Albums  []AlbumEntry `xml:"album" json:"album,omitempty"`
}

// AlbumList2 is the ID3-style variant Feishin browses by default.
// Same entry shape, different wrapper.
type AlbumList2 struct {
	XMLName xml.Name     `xml:"albumList2" json:"-"`
	Albums  []AlbumEntry `xml:"album" json:"album,omitempty"`
}

// AlbumInfo is the getAlbum.view payload: album + its track list.
type AlbumInfo struct {
	XMLName   xml.Name    `xml:"album" json:"-"`
	ID        string      `xml:"id,attr" json:"id"`
	Name      string      `xml:"name,attr" json:"name"`
	Artist    string      `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID  string      `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt  string      `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount int         `xml:"songCount,attr,omitempty" json:"songCount,omitempty"`
	Duration  int         `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	Songs     []SongEntry `xml:"song" json:"song,omitempty"`
}

// JukeboxStatus is a minimal jukeboxControl stub (no server-side player;
// always reports an empty stopped queue).
type JukeboxStatus struct {
	XMLName      xml.Name `xml:"jukeboxStatus" json:"-"`
	CurrentIndex int      `xml:"currentIndex,attr" json:"currentIndex"`
	Playing      bool     `xml:"playing,attr" json:"playing"`
	Gain         float64  `xml:"gain,attr" json:"gain"`
}

// ArtistInfo is the getArtist.view payload: artist + their albums.
type ArtistInfo struct {
	XMLName    xml.Name     `xml:"artist" json:"-"`
	ID         string       `xml:"id,attr" json:"id"`
	Name       string       `xml:"name,attr" json:"name"`
	CoverArt   string       `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	AlbumCount int          `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
	Albums     []AlbumEntry `xml:"album" json:"album,omitempty"`
}

type Starred struct {
	XMLName xml.Name    `xml:"starred" json:"-"`
	Songs   []SongEntry `xml:"song" json:"song,omitempty"`
}

type Starred2 struct {
	XMLName xml.Name    `xml:"starred2" json:"-"`
	Songs   []SongEntry `xml:"song" json:"song,omitempty"`
}

type SearchResult struct {
	XMLName xml.Name      `xml:"searchResult3" json:"-"`
	Songs   []SongEntry   `xml:"song" json:"song,omitempty"`
	Albums  []AlbumEntry  `xml:"album,omitempty" json:"album,omitempty"`
	Artists []ArtistEntry `xml:"artist,omitempty" json:"artist,omitempty"`
}

type SongEntry struct {
	XMLName     xml.Name `xml:"song" json:"-"`
	ID          string   `xml:"id,attr" json:"id"`
	Parent      string   `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool     `xml:"isDir,attr" json:"isDir"`
	Title       string   `xml:"title,attr" json:"title"`
	Album       string   `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string   `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int      `xml:"track,attr,omitempty" json:"track,omitempty"`
	Year        int      `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre       string   `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	CoverArt    string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration    int      `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	ContentType string   `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string   `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	BitRate     int      `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	Size        int64    `xml:"size,attr,omitempty" json:"size,omitempty"`
	Path        string   `xml:"path,attr,omitempty" json:"path,omitempty"`
	AlbumID     string   `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string   `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string   `xml:"type,attr,omitempty" json:"type,omitempty"`

	// OpenSubsonic audio properties (present for cached/local files where the
	// bytes can be inspected).
	SamplingRate int `xml:"samplingRate,attr,omitempty" json:"samplingRate,omitempty"`
	BitDepth     int `xml:"bitDepth,attr,omitempty" json:"bitDepth,omitempty"`
	ChannelCount int `xml:"channelCount,attr,omitempty" json:"channelCount,omitempty"`
}

type AlbumEntry struct {
	XMLName   xml.Name `xml:"album" json:"-"`
	ID        string   `xml:"id,attr" json:"id"`
	Name      string   `xml:"name,attr" json:"name"`
	Artist    string   `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID  string   `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt  string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount int      `xml:"songCount,attr,omitempty" json:"songCount,omitempty"`
	Duration  int      `xml:"duration,attr,omitempty" json:"duration,omitempty"`
}

type ArtistEntry struct {
	XMLName    xml.Name `xml:"artist" json:"-"`
	ID         string   `xml:"id,attr" json:"id"`
	Name       string   `xml:"name,attr" json:"name"`
	CoverArt   string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	AlbumCount int      `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
}

type MusicFolders struct {
	XMLName xml.Name `xml:"musicFolders" json:"-"`
	Folders []Folder `xml:"musicFolder" json:"musicFolder"`
}

type Folder struct {
	XMLName xml.Name `xml:"musicFolder" json:"-"`
	ID      int      `xml:"id,attr" json:"id"`
	Name    string   `xml:"name,attr" json:"name"`
}

type Artists struct {
	XMLName xml.Name      `xml:"artists" json:"-"`
	Ignored string        `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	List    []ArtistEntry `xml:"artist" json:"artist,omitempty"`
}

type Extensions struct {
	XMLName xml.Name    `xml:"openSubsonicExtensions" json:"-"`
	List    []Extension `xml:"extension" json:"extension,omitempty"`
}

type Extension struct {
	XMLName  xml.Name `xml:"extension" json:"-"`
	Name     string   `xml:"name,attr" json:"name"`
	Versions []int    `xml:"versions>version" json:"versions"`
}

type ServerInfo struct {
	XMLName       xml.Name `xml:"serverInfo" json:"-"`
	Version       string   `xml:"version,attr" json:"version"`
	ManagementURL string   `xml:"managementUrl,attr,omitempty" json:"managementUrl,omitempty"`
}

func okResponse() *Response {
	return &Response{
		Status: "ok", Version: restVersion,
		Type: serverType, ServerVersion: serverVersion, OpenSubsonic: true,
	}
}

func failResponse(code int, msg string) *Response {
	r := okResponse()
	r.Status = "failed"
	r.Error = &RespError{Code: code, Message: msg}
	return r
}

// write serializes resp honoring ?f=json|xml (default xml per spec, but most
// modern clients ask for json).
func write(w http.ResponseWriter, r *http.Request, resp *Response) {
	f := r.FormValue("f")
	if strings.EqualFold(f, "json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]*Response{"subsonic-response": resp})
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func writeFail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	write(w, r, failResponse(code, msg))
}
