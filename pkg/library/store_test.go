package library

import (
	"testing"
	"time"
)

func TestPlaylistLifecycleAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := s.CreatePlaylist("", "Road", "driving", "admin", false, []string{"a", "b", "a"})
	if p.ID == "" {
		t.Fatal("expected generated id")
	}
	if len(p.SongIDs) != 2 {
		t.Fatalf("song ids not deduped: %v", p.SongIDs)
	}

	// Reload from disk: state must survive a restart.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Playlist(p.ID)
	if !ok || got.Name != "Road" || len(got.SongIDs) != 2 {
		t.Fatalf("reloaded playlist = %+v ok=%v", got, ok)
	}

	name := "Roadtrip"
	updated, ok := s2.UpdatePlaylist(p.ID, &name, nil, nil, []string{"c"}, []int{0})
	if !ok {
		t.Fatal("update failed")
	}
	if updated.Name != "Roadtrip" {
		t.Fatalf("name = %q", updated.Name)
	}
	if len(updated.SongIDs) != 2 || updated.SongIDs[0] != "b" || updated.SongIDs[1] != "c" {
		t.Fatalf("updated songs = %v", updated.SongIDs)
	}

	if !s2.DeletePlaylist(p.ID) {
		t.Fatal("delete failed")
	}
	if _, ok := s2.Playlist(p.ID); ok {
		t.Fatal("playlist still present after delete")
	}
}

func TestCreatePlaylistByIDAppends(t *testing.T) {
	s, _ := New(t.TempDir())
	p := s.CreatePlaylist("fixed", "Mix", "", "admin", false, []string{"a"})
	p = s.CreatePlaylist("fixed", "", "", "admin", false, []string{"b"})
	if p.ID != "fixed" || len(p.SongIDs) != 2 || p.Name != "Mix" {
		t.Fatalf("append result = %+v", p)
	}
}

func TestStarsRatingsScrobbleQueue(t *testing.T) {
	s, _ := New(t.TempDir())

	s.Star(KindSong, "song1", true)
	s.Star(KindAlbum, "album1", true)
	s.Star(KindArtist, "artist1", true)
	if got := s.StarredSongs(); len(got) != 1 || got[0] != "song1" {
		t.Fatalf("starred songs = %v", got)
	}
	s.Star(KindSong, "song1", false)
	if got := s.StarredSongs(); len(got) != 0 {
		t.Fatalf("unstar failed: %v", got)
	}

	s.SetRating("song1", 4)
	if got := s.Rating("song1"); got != 4 {
		t.Fatalf("rating = %d", got)
	}
	s.SetRating("song1", 0)
	if got := s.Rating("song1"); got != 0 {
		t.Fatalf("cleared rating = %d", got)
	}

	s.Scrobble("song1", "admin", "test", false, time.Now().UTC())
	if got := s.NowPlaying(); len(got) != 1 || got[0].ID != "song1" {
		t.Fatalf("now playing = %+v", got)
	}
	s.Scrobble("song1", "admin", "test", true, time.Now().UTC())
	if got := s.NowPlaying(); len(got) != 0 {
		t.Fatalf("completed play should clear now playing: %+v", got)
	}
	if got := s.PlayCount("song1"); got != 1 {
		t.Fatalf("play count = %d", got)
	}

	s.SaveQueue(PlayQueue{SongIDs: []string{"a", "b"}, Current: "b", Position: 1234, User: "admin"})
	q, ok := s.Queue()
	if !ok || q.Current != "b" || q.Position != 1234 || len(q.SongIDs) != 2 {
		t.Fatalf("queue = %+v ok=%v", q, ok)
	}
}
