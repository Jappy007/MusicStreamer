package subsonic

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/musicstreamer/musicstreamer/pkg/audioinfo"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
)

// cachedSong is a track present in the disk cache, reconstructed from its
// cache key. Cache keys are exactly the reversible song IDs the API issues,
// so the cache doubles as a library catalog with no extra bookkeeping.
type cachedSong struct {
	id                   string
	artist, album, title string
	size                 int64
}

// catalog is an in-memory view of the disk cache grouped for browsing.
// It is rebuilt from the cache index on demand (O(entries), a few hundred
// bytes each) so it always reflects the current LRU state with no cache of
// its own to invalidate.
type catalog struct {
	s        *Server
	songs    []cachedSong
	byArtist map[string]map[string][]cachedSong // artist -> album -> songs
	artists  []string
}

func displayAlbum(album string) string {
	if strings.TrimSpace(album) == "" {
		return "Unknown Album"
	}
	return album
}

// catalog snapshots the disk cache into a browse tree.
func (s *Server) catalog() *catalog {
	c := &catalog{s: s, byArtist: make(map[string]map[string][]cachedSong)}
	if s.storage == nil {
		return c
	}
	seen := make(map[string]bool)
	for _, id := range s.storage.IDs() {
		if seen[id] {
			continue
		}
		seen[id] = true
		artist, album, title, err := metadata.SplitID(id)
		if err != nil || strings.TrimSpace(title) == "" {
			continue
		}
		if strings.TrimSpace(artist) == "" {
			artist = "Unknown Artist"
		}
		size, _ := s.storage.Size(id)
		cs := cachedSong{id: id, artist: artist, album: album, title: title, size: size}
		c.songs = append(c.songs, cs)
		am := c.byArtist[artist]
		if am == nil {
			am = make(map[string][]cachedSong)
			c.byArtist[artist] = am
		}
		am[album] = append(am[album], cs)
	}
	sort.Slice(c.songs, func(i, j int) bool { return lessSong(c.songs[i], c.songs[j]) })
	for a := range c.byArtist {
		c.artists = append(c.artists, a)
		for al := range c.byArtist[a] {
			g := c.byArtist[a][al]
			sort.Slice(g, func(i, j int) bool { return lessSong(g[i], g[j]) })
		}
	}
	sort.Slice(c.artists, func(i, j int) bool {
		return strings.ToLower(c.artists[i]) < strings.ToLower(c.artists[j])
	})
	return c
}

func lessSong(a, b cachedSong) bool {
	if !strings.EqualFold(a.album, b.album) {
		return strings.ToLower(a.album) < strings.ToLower(b.album)
	}
	if !strings.EqualFold(a.title, b.title) {
		return strings.ToLower(a.title) < strings.ToLower(b.title)
	}
	return a.id < b.id
}

// entry builds a Subsonic song entry for a cached track, probing the file for
// format / sample rate / bit depth (memoized per id+size).
func (c *catalog) entry(cs cachedSong) SongEntry {
	return c.s.cachedEntry(cs)
}

// cachedEntry renders a cached track, including OpenSubsonic audio properties
// discovered from the file bytes.
func (s *Server) cachedEntry(cs cachedSong) SongEntry {
	albumID := metadata.EncodeID(cs.artist, cs.album, "")
	e := SongEntry{
		ID: cs.id, Parent: albumID, IsDir: false,
		Title: cs.title, Album: displayAlbum(cs.album), Artist: cs.artist,
		CoverArt: albumID,
		Path:     fmt.Sprintf("%s/%s/%s", cs.artist, displayAlbum(cs.album), cs.title),
		AlbumID:  albumID, ArtistID: metadata.EncodeID(cs.artist, "", ""),
		Type: "music", Size: cs.size,
	}
	if s.storage != nil {
		info := s.fileInfo(cs.id, filepath.Join(s.storage.Dir(), cs.id), cs.size)
		e.Suffix = info.Suffix()
		e.ContentType = info.ContentType()
		e.SamplingRate = info.SampleRate
		e.BitDepth = info.BitDepth
		e.ChannelCount = info.Channels
		if info.BitRate > 0 {
			e.BitRate = info.BitRate
		}
	}
	return e
}

// artistEntries lists artists (with album counts) for getArtists/getIndexes.
func (c *catalog) artistEntries() []ArtistEntry {
	out := make([]ArtistEntry, 0, len(c.artists))
	for _, a := range c.artists {
		out = append(out, ArtistEntry{
			ID:         metadata.EncodeID(a, "", ""),
			Name:       a,
			CoverArt:   metadata.EncodeID(a, "", ""),
			AlbumCount: len(c.byArtist[a]),
		})
	}
	return out
}

// albumEntries lists every album (with song counts) for getAlbumList/2.
func (c *catalog) albumEntries() []AlbumEntry {
	var out []AlbumEntry
	for _, a := range c.artists {
		for _, al := range c.sortedAlbums(a) {
			out = append(out, c.albumEntry(a, al))
		}
	}
	return out
}

func (c *catalog) albumEntry(artist, album string) AlbumEntry {
	aid := metadata.EncodeID(artist, album, "")
	return AlbumEntry{
		ID: aid, Name: displayAlbum(album), Artist: artist,
		ArtistID:  metadata.EncodeID(artist, "", ""),
		CoverArt:  aid,
		SongCount: len(c.byArtist[artist][album]),
	}
}

// sortedAlbums returns an artist's albums in case-insensitive name order.
func (c *catalog) sortedAlbums(artist string) []string {
	am := c.byArtist[artist]
	names := make([]string, 0, len(am))
	for al := range am {
		names = append(names, al)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(displayAlbum(names[i])) < strings.ToLower(displayAlbum(names[j]))
	})
	return names
}

// artistAlbums returns the albums for one artist (getArtist).
func (c *catalog) artistAlbums(artist string) []AlbumEntry {
	var out []AlbumEntry
	for _, al := range c.sortedAlbums(artist) {
		out = append(out, c.albumEntry(artist, al))
	}
	return out
}

// albumSongs returns the songs for one artist+album (getAlbum).
func (c *catalog) albumSongs(artist, album string) []SongEntry {
	var out []SongEntry
	for _, cs := range c.byArtist[artist][album] {
		out = append(out, c.entry(cs))
	}
	return out
}

// searchSongs returns cached songs matching a free-text query (all terms must
// appear across artist/album/title).
func (c *catalog) searchSongs(query string) []SongEntry {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil
	}
	var out []SongEntry
	for _, cs := range c.songs {
		hay := strings.ToLower(cs.artist + " " + cs.album + " " + cs.title)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c.entry(cs))
		}
	}
	return out
}

// songForID returns the entry for a cached track with exactly this id.
func (c *catalog) songForID(id string) (SongEntry, bool) {
	for _, cs := range c.songs {
		if cs.id == id {
			return c.entry(cs), true
		}
	}
	return SongEntry{}, false
}

// fileInfo probes a cached file's audio properties, memoized by id and size
// (size changes only if the track is re-downloaded, invalidating the memo).
func (s *Server) fileInfo(id, path string, size int64) audioinfo.Info {
	s.infoMu.Lock()
	if e, ok := s.info[id]; ok && e.size == size {
		s.infoMu.Unlock()
		return e.info
	}
	s.infoMu.Unlock()

	info, err := audioinfo.ProbeFile(path)
	if err != nil {
		info = audioinfo.Info{}
	}
	s.infoMu.Lock()
	if s.info == nil {
		s.info = make(map[string]infoCache)
	}
	s.info[id] = infoCache{size: size, info: info}
	s.infoMu.Unlock()
	return info
}
