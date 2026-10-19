package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
)

// restVersion is the Subsonic API version this server implements.
const restVersion = "1.16.1"

// Identity reported to clients. Defaults to MusicStreamer, but main.go calls
// ConfigureIdentity to present as Navidrome so clients built for Navidrome
// enable their full feature set.
var (
	serverType    = "MusicStreamer"
	serverVersion = "1.0.0"
)

// ConfigureIdentity overrides the reported server type/version (the
// subsonic-response `type`/`serverVersion` attributes). Empty values are
// ignored.
func ConfigureIdentity(typ, version string) {
	if typ != "" {
		serverType = typ
	}
	if version != "" {
		serverVersion = version
	}
}

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

	Playlists    *Playlists         `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Playlist     *PlaylistWithSongs `xml:"playlist,omitempty" json:"playlist,omitempty"`
	Genres       *Genres            `xml:"genres,omitempty" json:"genres,omitempty"`
	RandomSongs  *RandomSongs       `xml:"randomSongs,omitempty" json:"randomSongs,omitempty"`
	SongsByGenre *Songs             `xml:"songsByGenre,omitempty" json:"songsByGenre,omitempty"`
	AlbumList    *AlbumList         `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2   *AlbumList2        `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	Album        *AlbumInfo         `xml:"album,omitempty" json:"album,omitempty"`
	Starred      *Starred           `xml:"starred,omitempty" json:"starred,omitempty"`
	Starred2     *Starred2          `xml:"starred2,omitempty" json:"starred2,omitempty"`
	Jukebox      *JukeboxStatus     `xml:"jukeboxStatus,omitempty" json:"jukeboxStatus,omitempty"`
	Artist       *ArtistInfo        `xml:"artist,omitempty" json:"artist,omitempty"`

	// Media annotation / playback state.
	NowPlaying *NowPlaying `xml:"nowPlaying,omitempty" json:"nowPlaying,omitempty"`
	PlayQueue  *PlayQueue  `xml:"playQueue,omitempty" json:"playQueue,omitempty"`
	ScanStatus *ScanStatus `xml:"scanStatus,omitempty" json:"scanStatus,omitempty"`

	// Info endpoints.
	ArtistInfo    *ArtistBio     `xml:"artistInfo,omitempty" json:"artistInfo,omitempty"`
	ArtistInfo2   *ArtistBio     `xml:"artistInfo2,omitempty" json:"artistInfo2,omitempty"`
	AlbumInfo     *AlbumBio      `xml:"albumInfo,omitempty" json:"albumInfo,omitempty"`
	AlbumInfo2    *AlbumBio      `xml:"albumInfo2,omitempty" json:"albumInfo2,omitempty"`
	SimilarSongs  *SimilarSongs  `xml:"similarSongs,omitempty" json:"similarSongs,omitempty"`
	SimilarSongs2 *SimilarSongs2 `xml:"similarSongs2,omitempty" json:"similarSongs2,omitempty"`
	TopSongs      *TopSongs      `xml:"topSongs,omitempty" json:"topSongs,omitempty"`
	Lyrics        *Lyrics        `xml:"lyrics,omitempty" json:"lyrics,omitempty"`
	LyricsList    *LyricsList    `xml:"lyricsList,omitempty" json:"lyricsList,omitempty"`

	// Browsing / misc (empty stubs where this server has no persistent store).
	MusicDirectory *Directory             `xml:"directory,omitempty" json:"directory,omitempty"`
	Indexes        *Indexes               `xml:"indexes,omitempty" json:"indexes,omitempty"`
	SearchResult2  *SearchResult2         `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	Users          *Users                 `xml:"users,omitempty" json:"users,omitempty"`
	Shares         *Shares                `xml:"shares,omitempty" json:"shares,omitempty"`
	Bookmarks      *Bookmarks             `xml:"bookmarks,omitempty" json:"bookmarks,omitempty"`
	RadioStations  *InternetRadioStations `xml:"internetRadioStations,omitempty" json:"internetRadioStations,omitempty"`
	Videos         *Videos                `xml:"videos,omitempty" json:"videos,omitempty"`
	Podcasts       *Podcasts              `xml:"podcasts,omitempty" json:"podcasts,omitempty"`
	ChatMessages   *ChatMessages          `xml:"chatMessages,omitempty" json:"chatMessages,omitempty"`

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
	Comment   string   `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	SongCount int      `xml:"songCount,attr" json:"songCount"`
	Duration  int      `xml:"duration,attr" json:"duration"`
	Public    bool     `xml:"public,attr" json:"public"`
	Owner     string   `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	CoverArt  string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	PlayCount int      `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	Created   string   `xml:"created,attr,omitempty" json:"created,omitempty"`
	Changed   string   `xml:"changed,attr,omitempty" json:"changed,omitempty"`
}

// PlaylistWithSongs is the getPlaylist.view payload: playlist + its entries.
type PlaylistWithSongs struct {
	XMLName   xml.Name    `xml:"playlist" json:"-"`
	ID        string      `xml:"id,attr" json:"id"`
	Name      string      `xml:"name,attr" json:"name"`
	Comment   string      `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Owner     string      `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	Public    bool        `xml:"public,attr" json:"public"`
	SongCount int         `xml:"songCount,attr" json:"songCount"`
	Duration  int         `xml:"duration,attr" json:"duration"`
	CoverArt  string      `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	PlayCount int         `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	Created   string      `xml:"created,attr,omitempty" json:"created,omitempty"`
	Changed   string      `xml:"changed,attr,omitempty" json:"changed,omitempty"`
	Entries   []SongEntry `xml:"entry" json:"entry,omitempty"`
}

// Genre is one entry of getGenres.view.
type Genre struct {
	XMLName    xml.Name `xml:"genre" json:"-"`
	Name       string   `xml:",chardata" json:"value"`
	SongCount  int      `xml:"songCount,attr,omitempty" json:"songCount,omitempty"`
	AlbumCount int      `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
}

type Genres struct {
	XMLName xml.Name `xml:"genres" json:"-"`
	Items   []Genre  `xml:"genre" json:"genre,omitempty"`
}

// Songs is a generic song-list wrapper (element named by the response field).
type Songs struct {
	Songs []SongEntry `xml:"song" json:"song,omitempty"`
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
	XMLName xml.Name      `xml:"starred" json:"-"`
	Artists []ArtistEntry `xml:"artist" json:"artist,omitempty"`
	Albums  []AlbumEntry  `xml:"album" json:"album,omitempty"`
	Songs   []SongEntry   `xml:"song" json:"song,omitempty"`
}

type Starred2 struct {
	XMLName xml.Name      `xml:"starred2" json:"-"`
	Artists []ArtistEntry `xml:"artist" json:"artist,omitempty"`
	Albums  []AlbumEntry  `xml:"album" json:"album,omitempty"`
	Songs   []SongEntry   `xml:"song" json:"song,omitempty"`
}

// SearchResult2 is the pre-ID3 search payload (search2.view).
type SearchResult2 struct {
	XMLName xml.Name      `xml:"searchResult2" json:"-"`
	Songs   []SongEntry   `xml:"song,omitempty" json:"song,omitempty"`
	Albums  []AlbumEntry  `xml:"album,omitempty" json:"album,omitempty"`
	Artists []ArtistEntry `xml:"artist,omitempty" json:"artist,omitempty"`
}

// ArtistBio is the getArtistInfo/getArtistInfo2 payload.
type ArtistBio struct {
	Biography      string        `xml:"biography,omitempty" json:"biography,omitempty"`
	MusicBrainzID  string        `xml:"musicBrainzId,omitempty" json:"musicBrainzId,omitempty"`
	LastFmURL      string        `xml:"lastFmUrl,omitempty" json:"lastFmUrl,omitempty"`
	SmallImageURL  string        `xml:"smallImageUrl,omitempty" json:"smallImageUrl,omitempty"`
	MediumImageURL string        `xml:"mediumImageUrl,omitempty" json:"mediumImageUrl,omitempty"`
	LargeImageURL  string        `xml:"largeImageUrl,omitempty" json:"largeImageUrl,omitempty"`
	SimilarArtists []ArtistEntry `xml:"similarArtist,omitempty" json:"similarArtist,omitempty"`
}

// AlbumBio is the getAlbumInfo/getAlbumInfo2 payload.
type AlbumBio struct {
	Notes          string `xml:"notes,omitempty" json:"notes,omitempty"`
	MusicBrainzID  string `xml:"musicBrainzId,omitempty" json:"musicBrainzId,omitempty"`
	LastFmURL      string `xml:"lastFmUrl,omitempty" json:"lastFmUrl,omitempty"`
	SmallImageURL  string `xml:"smallImageUrl,omitempty" json:"smallImageUrl,omitempty"`
	MediumImageURL string `xml:"mediumImageUrl,omitempty" json:"mediumImageUrl,omitempty"`
	LargeImageURL  string `xml:"largeImageUrl,omitempty" json:"largeImageUrl,omitempty"`
}

// SimilarSongs / SimilarSongs2 are empty when no similarity source is set.
type SimilarSongs struct {
	XMLName xml.Name    `xml:"similarSongs" json:"-"`
	Songs   []SongEntry `xml:"song" json:"song,omitempty"`
}

type SimilarSongs2 struct {
	XMLName xml.Name    `xml:"similarSongs2" json:"-"`
	Songs   []SongEntry `xml:"song" json:"song,omitempty"`
}

type TopSongs struct {
	XMLName xml.Name    `xml:"topSongs" json:"-"`
	Songs   []SongEntry `xml:"song" json:"song,omitempty"`
}

// Lyrics is the getLyrics.view payload.
type Lyrics struct {
	XMLName xml.Name `xml:"lyrics" json:"-"`
	Artist  string   `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Title   string   `xml:"title,attr,omitempty" json:"title,omitempty"`
	Value   string   `xml:",chardata" json:"value,omitempty"`
}

// LyricsList is the OpenSubsonic getLyricsBySongId payload.
type LyricsList struct {
	XMLName          xml.Name           `xml:"lyricsList" json:"-"`
	StructuredLyrics []StructuredLyrics `xml:"structuredLyrics" json:"structuredLyrics,omitempty"`
}

type StructuredLyrics struct {
	XMLName       xml.Name    `xml:"structuredLyrics" json:"-"`
	DisplayArtist string      `xml:"displayArtist,attr,omitempty" json:"displayArtist,omitempty"`
	DisplayTitle  string      `xml:"displayTitle,attr,omitempty" json:"displayTitle,omitempty"`
	Lang          string      `xml:"lang,attr,omitempty" json:"lang,omitempty"`
	Synced        bool        `xml:"synced,attr" json:"synced"`
	Line          []LyricLine `xml:"line" json:"line,omitempty"`
}

type LyricLine struct {
	XMLName xml.Name `xml:"line" json:"-"`
	Start   int64    `xml:"start,attr,omitempty" json:"start,omitempty"`
	Value   string   `xml:",chardata" json:"value,omitempty"`
}

// NowPlaying is the getNowPlaying.view payload.
type NowPlaying struct {
	XMLName xml.Name          `xml:"nowPlaying" json:"-"`
	Entries []NowPlayingEntry `xml:"entry" json:"entry,omitempty"`
}

// NowPlayingEntry is a song plus playback metadata.
type NowPlayingEntry struct {
	XMLName     xml.Name `xml:"entry" json:"-"`
	ID          string   `xml:"id,attr" json:"id"`
	Parent      string   `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool     `xml:"isDir,attr" json:"isDir"`
	Title       string   `xml:"title,attr" json:"title"`
	Album       string   `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string   `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int      `xml:"track,attr,omitempty" json:"track,omitempty"`
	CoverArt    string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration    int      `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	ContentType string   `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string   `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	Size        int64    `xml:"size,attr,omitempty" json:"size,omitempty"`
	AlbumID     string   `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string   `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string   `xml:"type,attr,omitempty" json:"type,omitempty"`
	Username    string   `xml:"username,attr,omitempty" json:"username,omitempty"`
	MinutesAgo  int      `xml:"minutesAgo,attr" json:"minutesAgo"`
	PlayerID    int      `xml:"playerId,attr" json:"playerId"`
	PlayerName  string   `xml:"playerName,attr,omitempty" json:"playerName,omitempty"`
}

// PlayQueue is the getPlayQueue.view payload.
type PlayQueue struct {
	XMLName   xml.Name    `xml:"playQueue" json:"-"`
	Entries   []SongEntry `xml:"entry" json:"entry,omitempty"`
	Current   string      `xml:"current,attr,omitempty" json:"current,omitempty"`
	Position  int64       `xml:"position,attr" json:"position"`
	Username  string      `xml:"username,attr,omitempty" json:"username,omitempty"`
	Changed   string      `xml:"changed,attr,omitempty" json:"changed,omitempty"`
	ChangedBy string      `xml:"changedBy,attr,omitempty" json:"changedBy,omitempty"`
}

// ScanStatus is the getScanStatus.view payload.
type ScanStatus struct {
	XMLName     xml.Name `xml:"scanStatus" json:"-"`
	Scanning    bool     `xml:"scanning,attr" json:"scanning"`
	Count       int64    `xml:"count,attr" json:"count"`
	LastScan    string   `xml:"lastScan,attr,omitempty" json:"lastScan,omitempty"`
	FolderCount int      `xml:"folderCount,attr,omitempty" json:"folderCount,omitempty"`
}

// Directory is the getMusicDirectory.view payload.
type Directory struct {
	XMLName  xml.Name         `xml:"directory" json:"-"`
	ID       string           `xml:"id,attr" json:"id"`
	Parent   string           `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	Name     string           `xml:"name,attr" json:"name"`
	Children []DirectoryChild `xml:"child" json:"child,omitempty"`
}

// DirectoryChild mirrors SongEntry but marshals as <child>.
type DirectoryChild struct {
	XMLName     xml.Name `xml:"child" json:"-"`
	ID          string   `xml:"id,attr" json:"id"`
	Parent      string   `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool     `xml:"isDir,attr" json:"isDir"`
	Title       string   `xml:"title,attr" json:"title"`
	Album       string   `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string   `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int      `xml:"track,attr,omitempty" json:"track,omitempty"`
	CoverArt    string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration    int      `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	Suffix      string   `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	ContentType string   `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Size        int64    `xml:"size,attr,omitempty" json:"size,omitempty"`
	AlbumID     string   `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string   `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string   `xml:"type,attr,omitempty" json:"type,omitempty"`
}

// Users is the getUsers.view payload.
type Users struct {
	XMLName xml.Name `xml:"users" json:"-"`
	Users   []User   `xml:"user" json:"user,omitempty"`
}

// Empty stubs for sharing / bookmarks / radio / video / podcasts / chat.
type Shares struct {
	XMLName xml.Name `xml:"shares" json:"-"`
	Shares  []Share  `xml:"share" json:"share,omitempty"`
}

type Share struct {
	XMLName     xml.Name    `xml:"share" json:"-"`
	ID          string      `xml:"id,attr" json:"id"`
	URL         string      `xml:"url,attr" json:"url"`
	Description string      `xml:"description,attr,omitempty" json:"description,omitempty"`
	Username    string      `xml:"username,attr" json:"username"`
	Created     string      `xml:"created,attr" json:"created"`
	Entries     []SongEntry `xml:"entry" json:"entry,omitempty"`
}

type Bookmarks struct {
	XMLName   xml.Name   `xml:"bookmarks" json:"-"`
	Bookmarks []Bookmark `xml:"bookmark" json:"bookmark,omitempty"`
}

type Bookmark struct {
	XMLName  xml.Name `xml:"bookmark" json:"-"`
	Position int64    `xml:"position,attr" json:"position"`
	Username string   `xml:"username,attr" json:"username"`
	Comment  string   `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Created  string   `xml:"created,attr" json:"created"`
	Changed  string   `xml:"changed,attr" json:"changed"`
}

type InternetRadioStations struct {
	XMLName  xml.Name               `xml:"internetRadioStations" json:"-"`
	Stations []InternetRadioStation `xml:"internetRadioStation" json:"internetRadioStation,omitempty"`
}

type InternetRadioStation struct {
	XMLName     xml.Name `xml:"internetRadioStation" json:"-"`
	ID          string   `xml:"id,attr" json:"id"`
	Name        string   `xml:"name,attr" json:"name"`
	StreamURL   string   `xml:"streamUrl,attr" json:"streamUrl"`
	HomepageURL string   `xml:"homePageUrl,attr,omitempty" json:"homePageUrl,omitempty"`
}

type Videos struct {
	XMLName xml.Name `xml:"videos" json:"-"`
	Videos  []Video  `xml:"video" json:"video,omitempty"`
}

type Video struct {
	XMLName   xml.Name `xml:"video" json:"-"`
	ID        string   `xml:"id,attr" json:"id"`
	Title     string   `xml:"title,attr" json:"title"`
	StreamURL string   `xml:"streamUrl,attr,omitempty" json:"streamUrl,omitempty"`
}

type Podcasts struct {
	XMLName  xml.Name         `xml:"podcasts" json:"-"`
	Channels []PodcastChannel `xml:"channel" json:"channel,omitempty"`
}

type PodcastChannel struct {
	XMLName xml.Name `xml:"channel" json:"-"`
	ID      string   `xml:"id,attr" json:"id"`
	URL     string   `xml:"url,attr" json:"url"`
	Title   string   `xml:"title,attr,omitempty" json:"title,omitempty"`
}

type ChatMessages struct {
	XMLName  xml.Name      `xml:"chatMessages" json:"-"`
	Messages []ChatMessage `xml:"chatMessage" json:"chatMessage,omitempty"`
}

type ChatMessage struct {
	XMLName  xml.Name `xml:"chatMessage" json:"-"`
	Username string   `xml:"username,attr" json:"username"`
	Time     int64    `xml:"time,attr" json:"time"`
	Message  string   `xml:"message,attr" json:"message"`
}

type SearchResult struct {
	XMLName xml.Name      `xml:"searchResult3" json:"-"`
	Songs   []SongEntry   `xml:"song,omitempty" json:"song,omitempty"`
	Albums  []AlbumEntry  `xml:"album,omitempty" json:"album,omitempty"`
	Artists []ArtistEntry `xml:"artist,omitempty" json:"artist,omitempty"`
}

// RandomSongs is the getRandomSongs.view payload: a flat song list.
type RandomSongs struct {
	XMLName xml.Name    `xml:"randomSongs" json:"-"`
	Songs   []SongEntry `xml:"song,omitempty" json:"song,omitempty"`
}

// SongEntry is a track. It deliberately has no XMLName: the same struct is
// marshalled as <song> or <entry> depending on the enclosing list.
type SongEntry struct {
	ID          string `xml:"id,attr" json:"id"`
	Parent      string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool   `xml:"isDir,attr" json:"isDir"`
	Title       string `xml:"title,attr" json:"title"`
	Album       string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	Year        int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre       string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	CoverArt    string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration    int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	ContentType string `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	BitRate     int    `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	Size        int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	Path        string `xml:"path,attr,omitempty" json:"path,omitempty"`
	AlbumID     string `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string `xml:"type,attr,omitempty" json:"type,omitempty"`

	// User state (populated for cached/local tracks).
	PlayCount  int `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	UserRating int `xml:"userRating,attr,omitempty" json:"userRating,omitempty"`

	// OpenSubsonic audio properties (present for cached/local files where the
	// bytes can be inspected).
	SamplingRate int `xml:"samplingRate,attr,omitempty" json:"samplingRate,omitempty"`
	BitDepth     int `xml:"bitDepth,attr,omitempty" json:"bitDepth,omitempty"`
	ChannelCount int `xml:"channelCount,attr,omitempty" json:"channelCount,omitempty"`
}

type AlbumEntry struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	Artist     string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID   string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt   string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount  int    `xml:"songCount,attr,omitempty" json:"songCount,omitempty"`
	Duration   int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	PlayCount  int    `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	UserRating int    `xml:"userRating,attr,omitempty" json:"userRating,omitempty"`
}

type ArtistEntry struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	CoverArt   string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	AlbumCount int    `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
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
	XMLName xml.Name `xml:"artists" json:"-"`
	Ignored string   `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Index   []Index  `xml:"index" json:"index,omitempty"`
}

// Indexes is the getIndexes.view payload (artists grouped by initial).
type Indexes struct {
	XMLName xml.Name `xml:"indexes" json:"-"`
	Ignored string   `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Index   []Index  `xml:"index" json:"index,omitempty"`
}

type Index struct {
	XMLName xml.Name      `xml:"index" json:"-"`
	Name    string        `xml:"name,attr" json:"name"`
	Artists []ArtistEntry `xml:"artist" json:"artist,omitempty"`
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
