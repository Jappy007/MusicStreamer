package subsonic

import (
	"log"
	"net/http"
	"time"
)

// Route registers every Subsonic endpoint on mux for both GET and POST,
// with and without the .view suffix style used by various clients.
// Only the five required endpoints carry real logic; the rest are compat
// stubs so Symfonium/Feishin don't hard-fail on library browse.
func Route(mux *http.ServeMux, s *Server) {
	bind := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)         // GET+POST
		mux.HandleFunc(pattern+".view", h) // some routers strip suffix first
	}

	// Required.
	bind("/rest/ping", s.Ping)
	bind("/rest/getLicense", s.License)
	bind("/rest/search3", s.Search3)
	bind("/rest/getSong", s.GetSong)
	bind("/rest/stream", s.Stream)

	// Compat stubs (keep native clients happy).
	bind("/rest/getMusicFolders", s.MusicFolders)
	bind("/rest/getUser", s.GetUser)
	bind("/rest/getIndexes", s.Indexes)
	bind("/rest/getArtists", s.Indexes)
	bind("/rest/getAlbumList", s.AlbumList)
	bind("/rest/getAlbumList2", s.AlbumList2)
	bind("/rest/getRandomSongs", s.RandomSongs)
	bind("/rest/getAlbum", s.GetAlbum)
	bind("/rest/getArtist", s.GetArtist)
	bind("/rest/getCoverArt", s.CoverArt)
	bind("/rest/jukeboxControl", s.Jukebox)
	bind("/rest/scrobble", s.Scrobble)
	bind("/rest/getPlaylists", s.Playlists)
	bind("/rest/getGenres", s.Genres)
	bind("/rest/getStarred", s.Starred)
	bind("/rest/getStarred2", s.Starred2)
	bind("/rest/getOpenSubsonicExtensions", s.OpenExtensions)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// WithAccessLog wraps h with single-line access logging for diagnosing
// client issues (Feishin/Symfonium login failures, etc.).
// Secrets are never printed: only the auth scheme and user are logged.
func WithAccessLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(sr, r)
		q := r.URL.Query()
		scheme := "none"
		switch {
		case q.Get("t") != "" && q.Get("s") != "":
			scheme = "token"
		case q.Get("p") != "":
			scheme = "password"
		}
		log.Printf("access: %s %s u=%q auth=%s -> %d (%s)",
			r.Method, r.URL.Path, q.Get("u"), scheme, sr.code, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

// WithCORS allows browser/web-based clients (Feishin web, Symfonium
// wrappers, reverse-proxied setups) to call the API cross-origin.
// Native apps are unaffected; preflights are answered without auth.
func WithCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}
