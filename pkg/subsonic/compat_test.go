package subsonic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/musicstreamer/musicstreamer/pkg/cache"
	"github.com/musicstreamer/musicstreamer/pkg/library"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
)

func newCompatServer(t *testing.T) *Server {
	t.Helper()
	lib, err := library.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{user: "admin", pass: "admin", lib: lib}
}

func serve(s *Server, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	Route(mux, s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// envelope decodes the parts of subsonic-response the tests inspect.
type envelope struct {
	Resp struct {
		Status        string `json:"status"`
		Type          string `json:"type"`
		ServerVersion string `json:"serverVersion"`
		Playlist      struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SongCount int    `json:"songCount"`
			Entry     []struct {
				ID string `json:"id"`
			} `json:"entry"`
		} `json:"playlist"`
		Playlists struct {
			Playlist []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"playlist"`
		} `json:"playlists"`
		Starred2 struct {
			Song []struct {
				ID string `json:"id"`
			} `json:"song"`
		} `json:"starred2"`
		NowPlaying struct {
			Entry []struct {
				ID string `json:"id"`
			} `json:"entry"`
		} `json:"nowPlaying"`
		PlayQueue struct {
			Current string `json:"current"`
		} `json:"playQueue"`
		Song struct {
			ID         string `json:"id"`
			UserRating int    `json:"userRating"`
			PlayCount  int    `json:"playCount"`
		} `json:"song"`
	} `json:"subsonic-response"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if e.Resp.Status != "ok" {
		t.Fatalf("status = %q body=%s", e.Resp.Status, rec.Body.String())
	}
	return e
}

func TestIdentityReportsNavidrome(t *testing.T) {
	ConfigureIdentity("navidrome", "0.55.0")
	s := newCompatServer(t)
	e := decode(t, serve(s, "/rest/ping?u=admin&p=admin&f=json"))
	if e.Resp.Type != "navidrome" || e.Resp.ServerVersion != "0.55.0" {
		t.Fatalf("identity = %q %q", e.Resp.Type, e.Resp.ServerVersion)
	}
}

func TestPlaylistEndpoints(t *testing.T) {
	s := newCompatServer(t)
	songA := metadata.EncodeID("Artist", "Album", "A")
	songB := metadata.EncodeID("Artist", "Album", "B")

	rec := serve(s, "/rest/createPlaylist?u=admin&p=admin&f=json&name=Test&songId="+songA+"&songId="+songB)
	e := decode(t, rec)
	id := e.Resp.Playlist.ID
	if id == "" || e.Resp.Playlist.SongCount != 2 {
		t.Fatalf("created playlist = %+v", e.Resp.Playlist)
	}

	e = decode(t, serve(s, "/rest/getPlaylists?u=admin&p=admin&f=json"))
	if len(e.Resp.Playlists.Playlist) != 1 || e.Resp.Playlists.Playlist[0].Name != "Test" {
		t.Fatalf("playlists = %+v", e.Resp.Playlists.Playlist)
	}

	e = decode(t, serve(s, "/rest/getPlaylist?u=admin&p=admin&f=json&id="+id))
	if len(e.Resp.Playlist.Entry) != 2 {
		t.Fatalf("playlist entries = %+v", e.Resp.Playlist.Entry)
	}

	// XML must use <entry> for playlist songs, not <song>.
	rec = serve(s, "/rest/getPlaylist?u=admin&p=admin&f=xml&id="+id)
	body := rec.Body.String()
	if !strings.Contains(body, "<entry ") || strings.Contains(body, "<song ") {
		t.Fatalf("unexpected XML: %s", body)
	}

	e = decode(t, serve(s, "/rest/updatePlaylist?u=admin&p=admin&f=json&playlistId="+id+"&name=Renamed&songIdToAdd="+metadata.EncodeID("Artist", "Album", "C")))
	if e.Resp.Status != "ok" {
		t.Fatal("update failed")
	}
	e = decode(t, serve(s, "/rest/getPlaylist?u=admin&p=admin&f=json&id="+id))
	if e.Resp.Playlist.Name != "Renamed" || e.Resp.Playlist.SongCount != 3 {
		t.Fatalf("after update = %+v", e.Resp.Playlist)
	}

	decode(t, serve(s, "/rest/deletePlaylist?u=admin&p=admin&f=json&id="+id))
	e = decode(t, serve(s, "/rest/getPlaylists?u=admin&p=admin&f=json"))
	if len(e.Resp.Playlists.Playlist) != 0 {
		t.Fatalf("playlist not deleted: %+v", e.Resp.Playlists.Playlist)
	}
}

func TestStarsNowPlayingAndQueue(t *testing.T) {
	s := newCompatServer(t)
	song := metadata.EncodeID("Artist", "Album", "Song")
	album := metadata.EncodeID("Artist", "Album", "")
	artist := metadata.EncodeID("Artist", "", "")

	decode(t, serve(s, "/rest/star?u=admin&p=admin&f=json&id="+song+"&albumId="+album+"&artistId="+artist))
	e := decode(t, serve(s, "/rest/getStarred2?u=admin&p=admin&f=json"))
	if len(e.Resp.Starred2.Song) != 1 || e.Resp.Starred2.Song[0].ID != song {
		t.Fatalf("starred2 = %+v", e.Resp.Starred2)
	}

	decode(t, serve(s, "/rest/scrobble?u=admin&p=admin&f=json&id="+song+"&submission=false"))
	e = decode(t, serve(s, "/rest/getNowPlaying?u=admin&p=admin&f=json"))
	if len(e.Resp.NowPlaying.Entry) != 1 || e.Resp.NowPlaying.Entry[0].ID != song {
		t.Fatalf("now playing = %+v", e.Resp.NowPlaying)
	}

	decode(t, serve(s, "/rest/savePlayQueue?u=admin&p=admin&f=json&id="+song+"&current="+song+"&position=42"))
	e = decode(t, serve(s, "/rest/getPlayQueue?u=admin&p=admin&f=json"))
	if e.Resp.PlayQueue.Current != song {
		t.Fatalf("queue current = %q", e.Resp.PlayQueue.Current)
	}

	decode(t, serve(s, "/rest/unstar?u=admin&p=admin&f=json&id="+song))
	e = decode(t, serve(s, "/rest/getStarred2?u=admin&p=admin&f=json"))
	if len(e.Resp.Starred2.Song) != 0 {
		t.Fatalf("unstar failed: %+v", e.Resp.Starred2.Song)
	}
}

// getSong must expose persisted state so clients can refresh a cached track
// after rating or scrobbling it.
func TestGetSongIncludesStoredState(t *testing.T) {
	s := newCompatServer(t)
	cm, err := cache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := metadata.EncodeID("Artist", "Album", "Song")
	if err := cm.Store(id, strings.NewReader("not real audio")); err != nil {
		t.Fatal(err)
	}
	s.storage = cm

	decode(t, serve(s, "/rest/setRating?u=admin&p=admin&f=json&id="+id+"&rating=5"))
	decode(t, serve(s, "/rest/scrobble?u=admin&p=admin&f=json&id="+id+"&submission=true"))

	e := decode(t, serve(s, "/rest/getSong?u=admin&p=admin&f=json&id="+id))
	if e.Resp.Song.ID != id || e.Resp.Song.UserRating != 5 || e.Resp.Song.PlayCount != 1 {
		t.Fatalf("song = %+v", e.Resp.Song)
	}
}
