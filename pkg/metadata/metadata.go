package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Song is the canonical in-memory track identity. The ID is a reversible,
// filesystem-safe encoding of artist/album/title so the server stays
// stateless (no DB) while stream.view can still resolve Tier 2/3.
type Song struct {
	ID       string `json:"id"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Title    string `json:"title"`
	Duration int    `json:"duration"` // seconds
	CoverURL string `json:"coverUrl,omitempty"`
	Track    int    `json:"track,omitempty"`
	AlbumID  string `json:"albumId,omitempty"`
	ArtistID string `json:"artistId,omitempty"`
}

// EncodeID builds a reversible opaque Subsonic song ID.
func EncodeID(artist, album, title string) string {
	raw := artist + "\x1f" + album + "\x1f" + title
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeID reverses EncodeID.
func DecodeID(id string) (artist, album, title string, err error) {
	artist, album, title, err = SplitID(id)
	if err != nil {
		return "", "", "", err
	}
	if title == "" {
		return "", "", "", fmt.Errorf("metadata: malformed song id")
	}
	return artist, album, title, nil
}

// SplitID decodes any ID issued by EncodeID, tolerating empty segments.
// Album/artist IDs (used as getAlbum/getCoverArt references) encode empty
// trailing segments, so they need this lenient form. DecodeID stays strict
// for stream/getSong, which require a real track title.
func SplitID(id string) (artist, album, title string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(id))
	if err != nil {
		return "", "", "", fmt.Errorf("metadata: bad song id: %w", err)
	}
	parts := strings.SplitN(string(b), "\x1f", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("metadata: malformed song id")
	}
	return parts[0], parts[1], parts[2], nil
}

// Client searches public metadata (Deezer primary, MusicBrainz fallback).
type Client struct {
	http *http.Client
}

// New builds a metadata client with its own timeout budget.
func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{http: httpClient}
}

// Search returns up to limit songs for query, trying Deezer first.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Song, error) {
	if c == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if songs, err := c.searchDeezer(ctx, query, limit); err == nil && len(songs) > 0 {
		return songs, nil
	}
	return c.searchMusicBrainz(ctx, query, limit)
}

type deezerResp struct {
	Data []struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		Duration      int    `json:"duration"`
		TrackPosition int    `json:"track_position"`
		Artist        struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
		Album struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Cover string `json:"cover_medium"`
		} `json:"album"`
	} `json:"data"`
}

func (c *Client) searchDeezer(ctx context.Context, query string, limit int) ([]Song, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	u := "https://api.deezer.com/search?q=" + url.QueryEscape(query) +
		fmt.Sprintf("&limit=%d", limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var dz deezerResp
	if err := json.Unmarshal(body, &dz); err != nil {
		return nil, err
	}
	out := make([]Song, 0, len(dz.Data))
	for _, d := range dz.Data {
		out = append(out, Song{
			ID:       EncodeID(d.Artist.Name, d.Album.Title, d.Title),
			Artist:   d.Artist.Name,
			Album:    d.Album.Title,
			Title:    d.Title,
			Duration: d.Duration,
			CoverURL: d.Album.Cover,
			Track:    d.TrackPosition,
			AlbumID:  EncodeID(d.Artist.Name, d.Album.Title, ""),
			ArtistID: EncodeID(d.Artist.Name, "", ""),
		})
	}
	return out, nil
}

type mbResp struct {
	Recordings []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Length       int    `json:"length"` // ms
		ArtistCredit []struct {
			Name string `json:"name"`
		} `json:"artist-credit"`
		Releases []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"releases"`
	} `json:"recordings"`
}

func (c *Client) searchMusicBrainz(ctx context.Context, query string, limit int) ([]Song, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	u := "https://musicbrainz.org/ws/2/recording/?query=" + url.QueryEscape(query) +
		fmt.Sprintf("&limit=%d&fmt=json", limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MusicStreamer/1.0 (stateless-subsonic)")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musicbrainz: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var mb mbResp
	if err := json.Unmarshal(body, &mb); err != nil {
		return nil, err
	}
	out := make([]Song, 0, len(mb.Recordings))
	for _, r := range mb.Recordings {
		artist := ""
		if len(r.ArtistCredit) > 0 {
			artist = r.ArtistCredit[0].Name
		}
		album := ""
		if len(r.Releases) > 0 {
			album = r.Releases[0].Title
		}
		out = append(out, Song{
			ID:       EncodeID(artist, album, r.Title),
			Artist:   artist,
			Album:    album,
			Title:    r.Title,
			Duration: r.Length / 1000,
		})
	}
	return out, nil
}
