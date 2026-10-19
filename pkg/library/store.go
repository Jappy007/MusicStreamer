// Package library is a tiny, stdlib-only persistent store for the mutable
// user state a Subsonic/Navidrome client expects: playlists, starred items,
// ratings, play counts and the play queue.
//
// State is a single JSON file rewritten atomically (tmp + rename). This keeps
// the server database-free while giving clients persistent playlists and
// favourites — the main reason players built for Navidrome "don't work 100%"
// against a stateless server.
package library

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Starrable entity kinds (used with Star and the starred lists).
const (
	KindSong   = "song"
	KindAlbum  = "album"
	KindArtist = "artist"
)

// Playlist is a named, ordered list of song IDs.
type Playlist struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Comment string    `json:"comment,omitempty"`
	Owner   string    `json:"owner,omitempty"`
	Public  bool      `json:"public"`
	SongIDs []string  `json:"songIds"`
	Created time.Time `json:"created"`
	Changed time.Time `json:"changed"`
}

// NowPlaying is one active playback report (scrobble with submission=false).
type NowPlaying struct {
	ID      string    `json:"id"`
	User    string    `json:"user,omitempty"`
	Player  string    `json:"player,omitempty"`
	Started time.Time `json:"started"`
}

// PlayQueue mirrors a client's active play queue (savePlayQueue/getPlayQueue).
type PlayQueue struct {
	SongIDs  []string  `json:"songIds,omitempty"`
	Current  string    `json:"current,omitempty"`
	Position int64     `json:"position,omitempty"`
	User     string    `json:"user,omitempty"`
	Changed  time.Time `json:"changed"`
}

type state struct {
	Playlists   []Playlist           `json:"playlists"`
	StarSongs   []string             `json:"starredSongs,omitempty"`
	StarAlbums  []string             `json:"starredAlbums,omitempty"`
	StarArtists []string             `json:"starredArtists,omitempty"`
	Ratings     map[string]int       `json:"ratings,omitempty"`
	Plays       map[string]int       `json:"playCounts,omitempty"`
	PlayedAt    map[string]time.Time `json:"playedAt,omitempty"`
	NowPlaying  []NowPlaying         `json:"nowPlaying,omitempty"`
	Queue       *PlayQueue           `json:"queue,omitempty"`
}

// Store is a concurrency-safe, file-backed user-state store. All mutations
// persist synchronously under the lock; the file is tiny (a few KiB).
type Store struct {
	path string
	mu   sync.Mutex
	data state
}

// New loads (or creates) the store whose JSON lives at dir/library.json.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("library: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "library.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.data) // tolerate a corrupt file: start fresh
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.data.Ratings == nil {
		s.data.Ratings = make(map[string]int)
	}
	if s.data.Plays == nil {
		s.data.Plays = make(map[string]int)
	}
	if s.data.PlayedAt == nil {
		s.data.PlayedAt = make(map[string]time.Time)
	}
	return s, nil
}

// persist atomically rewrites the state file. Callers must hold s.mu.
func (s *Store) persist() {
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// --- playlists ------------------------------------------------------------

// Playlists returns a snapshot copy of every playlist.
func (s *Store) Playlists() []Playlist {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Playlist, len(s.data.Playlists))
	copy(out, s.data.Playlists)
	return out
}

// Playlist returns one playlist by ID.
func (s *Store) Playlist(id string) (Playlist, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.data.Playlists {
		if p.ID == id {
			return p, true
		}
	}
	return Playlist{}, false
}

// CreatePlaylist creates a playlist, or appends songs to an existing one when
// id matches (createPlaylist with a playlistId). name/comment are applied only
// when non-empty, so a pure append never clobbers them.
func (s *Store) CreatePlaylist(id, name, comment, owner string, public bool, songIDs []string) Playlist {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if id == "" {
		id = newID()
	}
	for i := range s.data.Playlists {
		if s.data.Playlists[i].ID == id {
			p := &s.data.Playlists[i]
			if name != "" {
				p.Name = name
			}
			if comment != "" {
				p.Comment = comment
			}
			p.Public = public
			p.SongIDs = append(p.SongIDs, songIDs...)
			p.Changed = now
			s.persist()
			return *p
		}
	}
	p := Playlist{
		ID: id, Name: name, Comment: comment, Owner: owner,
		Public: public, SongIDs: dedupe(songIDs), Created: now, Changed: now,
	}
	s.data.Playlists = append(s.data.Playlists, p)
	s.persist()
	return p
}

// UpdatePlaylist edits name/comment/public, appends songs and removes songs by
// index (per the Subsonic updatePlaylist contract).
func (s *Store) UpdatePlaylist(id string, name, comment *string, public *bool, add []string, removeIdx []int) (Playlist, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Playlists {
		p := &s.data.Playlists[i]
		if p.ID != id {
			continue
		}
		if name != nil {
			p.Name = *name
		}
		if comment != nil {
			p.Comment = *comment
		}
		if public != nil {
			p.Public = *public
		}
		if len(add) > 0 {
			p.SongIDs = append(p.SongIDs, add...)
		}
		if len(removeIdx) > 0 {
			rm := make(map[int]bool, len(removeIdx))
			for _, idx := range removeIdx {
				rm[idx] = true
			}
			kept := make([]string, 0, len(p.SongIDs))
			for j, sid := range p.SongIDs {
				if !rm[j] {
					kept = append(kept, sid)
				}
			}
			p.SongIDs = kept
		}
		p.Changed = time.Now().UTC()
		s.persist()
		return *p, true
	}
	return Playlist{}, false
}

// DeletePlaylist removes a playlist.
func (s *Store) DeletePlaylist(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Playlists {
		if s.data.Playlists[i].ID == id {
			s.data.Playlists = append(s.data.Playlists[:i], s.data.Playlists[i+1:]...)
			s.persist()
			return true
		}
	}
	return false
}

// --- stars ----------------------------------------------------------------

// Star adds or removes id from the starred set of the given kind.
func (s *Store) Star(kind, id string, on bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case KindSong:
		s.data.StarSongs = setMember(s.data.StarSongs, id, on)
	case KindAlbum:
		s.data.StarAlbums = setMember(s.data.StarAlbums, id, on)
	case KindArtist:
		s.data.StarArtists = setMember(s.data.StarArtists, id, on)
	default:
		return
	}
	s.persist()
}

func (s *Store) StarredSongs() []string   { return s.starred(&s.data.StarSongs) }
func (s *Store) StarredAlbums() []string  { return s.starred(&s.data.StarAlbums) }
func (s *Store) StarredArtists() []string { return s.starred(&s.data.StarArtists) }

func (s *Store) starred(ptr *[]string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(*ptr))
	copy(out, *ptr)
	return out
}

// --- ratings --------------------------------------------------------------

// SetRating stores a 1..5 rating; 0 clears it.
func (s *Store) SetRating(id string, rating int) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rating <= 0 {
		delete(s.data.Ratings, id)
	} else {
		if rating > 5 {
			rating = 5
		}
		s.data.Ratings[id] = rating
	}
	s.persist()
}

// Rating returns the stored rating for id (0 if none).
func (s *Store) Rating(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Ratings[id]
}

// Ratings returns a copy of every rating.
func (s *Store) Ratings() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.data.Ratings))
	for k, v := range s.data.Ratings {
		out[k] = v
	}
	return out
}

// --- play tracking --------------------------------------------------------

// Scrobble records playback. submission=false means "now playing"; true means
// a completed play (increments the play count and clears the now-playing row).
func (s *Store) Scrobble(id, user, player string, submission bool, at time.Time) {
	if id == "" {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !submission {
		s.data.NowPlaying = upsertNowPlaying(s.data.NowPlaying, NowPlaying{
			ID: id, User: user, Player: player, Started: at,
		})
		s.persist()
		return
	}
	out := s.data.NowPlaying[:0:0]
	for _, np := range s.data.NowPlaying {
		if np.ID != id {
			out = append(out, np)
		}
	}
	s.data.NowPlaying = out
	s.data.Plays[id]++
	s.data.PlayedAt[id] = at
	s.persist()
}

// nowPlayingTTL bounds how long a now-playing row is reported, so a client
// that never sends a completion scrobble does not leave a track "playing"
// forever.
const nowPlayingTTL = 30 * time.Minute

// NowPlaying returns active playback rows, newest first, dropping stale ones.
func (s *Store) NowPlaying() []NowPlaying {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	out := make([]NowPlaying, 0, len(s.data.NowPlaying))
	for _, np := range s.data.NowPlaying {
		if now.Sub(np.Started) <= nowPlayingTTL {
			out = append(out, np)
		}
	}
	return out
}

// PlayCount returns the number of completed plays for id.
func (s *Store) PlayCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Plays[id]
}

// LastPlayed returns when id was last fully played (zero if never).
func (s *Store) LastPlayed(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.PlayedAt[id]
}

// --- play queue -----------------------------------------------------------

// SaveQueue stores a client's active play queue.
func (s *Store) SaveQueue(q PlayQueue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q.Changed = time.Now().UTC()
	q.SongIDs = dedupe(q.SongIDs)
	s.data.Queue = &q
	s.persist()
}

// Queue returns the stored play queue, if any.
func (s *Store) Queue() (PlayQueue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Queue == nil {
		return PlayQueue{}, false
	}
	return *s.data.Queue, true
}

// --- helpers --------------------------------------------------------------

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

func setMember(list []string, id string, on bool) []string {
	out := make([]string, 0, len(list)+1)
	found := false
	for _, v := range list {
		if v == id {
			found = true
			if !on {
				continue
			}
		}
		out = append(out, v)
	}
	if on && !found {
		out = append(out, id)
	}
	return out
}

func dedupe(list []string) []string {
	if len(list) == 0 {
		return list
	}
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func upsertNowPlaying(list []NowPlaying, np NowPlaying) []NowPlaying {
	for i := range list {
		if list[i].ID == np.ID {
			list[i] = np
			return list
		}
	}
	return append(list, np)
}
