package subsonic

import (
	"fmt"
	"sort"
	"strings"

	"github.com/debridsubsonic/debridsubsonic/pkg/metadata"
)

// cachedSong is a track present in the disk cache, reconstructed from its
// cache key. Cache keys are exactly the reversible song IDs the API issues,
// so the cache doubles as a library catalog with no extra bookkeeping.
type cachedSong struct {
	id                   string
	artist, album, title string
}

func (c cachedSong) entry() SongEntry {
	return SongEntry{
		ID: c.id, Parent: metadata.EncodeID(c.artist, c.album, ""), IsDir: false,
		Title: c.title, Album: displayAlbum(c.album), Artist: c.artist,
		CoverArt: metadata.EncodeID(c.artist, c.album, ""),
		Path:     fmt.Sprintf("%s/%s/%s", c.artist, displayAlbum(c.album), c.title),
		AlbumID:  metadata.EncodeID(c.artist, c.album, ""),
		ArtistID: metadata.EncodeID(c.artist, "", ""),
		Type:     "music",
	}
}

// catalog is an in-memory view of the disk cache grouped for browsing.
// It is rebuilt from the cache index on demand (O(entries), a few hundred
// bytes each) so it always reflects the current LRU state with no cache of
// its own to invalidate.
type catalog struct {
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
	c := &catalog{byArtist: make(map[string]map[string][]cachedSong)}
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
		cs := cachedSong{id: id, artist: artist, album: album, title: title}
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
		out = append(out, cs.entry())
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
			out = append(out, cs.entry())
		}
	}
	return out
}

// has reports whether the cache holds a track with exactly this id.
func (c *catalog) has(id string) (cachedSong, bool) {
	for _, cs := range c.songs {
		if cs.id == id {
			return cs, true
		}
	}
	return cachedSong{}, false
}
