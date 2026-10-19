package subsonic

import (
	"log"
	"net/http"
	"time"
)

// Route registers the Subsonic/OpenSubsonic endpoints on mux for both GET and
// POST, with and without the .view suffix style used by various clients.
// Playlists, favourites, ratings and playback state are persisted; features
// the server has no backing store for (shares, radio, podcasts, chat) return
// valid empty responses so clients do not fail on them.
func Route(mux *http.ServeMux, s *Server) {
	bind := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)         // GET+POST
		mux.HandleFunc(pattern+".view", h) // some routers strip suffix first
	}

	// Navidrome native API (used by Psysonic and other Navidrome-first clients).
	mux.HandleFunc("/auth/login", s.NativeLogin)
	mux.HandleFunc("/api/keepalive/keepalive", s.NativeKeepalive)
	mux.HandleFunc("/api/song", s.NativeSongs)
	mux.HandleFunc("/api/song/", s.NativeSongSub)
	mux.HandleFunc("/api/album", s.NativeAlbums)
	mux.HandleFunc("/api/album/", s.NativeAlbumSub)
	mux.HandleFunc("/api/artist", s.NativeArtists)
	mux.HandleFunc("/api/artist/", s.NativeArtistSub)
	mux.HandleFunc("/api/library", s.NativeLibraries)
	mux.HandleFunc("/api/playlist/", s.NativeMissingImage)
	mux.HandleFunc("/api/radio/", s.NativeMissingImage)

	// Required.
	bind("/rest/ping", s.Ping)
	bind("/rest/getLicense", s.License)
	bind("/rest/search3", s.Search3)
	bind("/rest/getSong", s.GetSong)
	bind("/rest/stream", s.Stream)

	// Browsing.
	bind("/rest/getMusicFolders", s.MusicFolders)
	bind("/rest/getUser", s.GetUser)
	bind("/rest/getUsers", s.GetUsers)
	bind("/rest/getAvatar", s.GetAvatar)
	bind("/rest/getIndexes", s.GetIndexes)
	bind("/rest/getArtists", s.Indexes)
	bind("/rest/getAlbumList", s.AlbumList)
	bind("/rest/getAlbumList2", s.AlbumList2)
	bind("/rest/getRandomSongs", s.RandomSongs)
	bind("/rest/getAlbum", s.GetAlbum)
	bind("/rest/getArtist", s.GetArtist)
	bind("/rest/getMusicDirectory", s.GetMusicDirectory)
	bind("/rest/getCoverArt", s.CoverArt)
	bind("/rest/download", s.Download)
	bind("/rest/search2", s.Search2)

	// Artist / album info + lyrics.
	bind("/rest/getArtistInfo", s.GetArtistInfo)
	bind("/rest/getArtistInfo2", s.GetArtistInfo2)
	bind("/rest/getAlbumInfo", s.GetAlbumInfo)
	bind("/rest/getAlbumInfo2", s.GetAlbumInfo2)
	bind("/rest/getLyrics", s.GetLyrics)
	bind("/rest/getLyricsBySongId", s.GetLyricsBySongId)

	// Playlists.
	bind("/rest/getPlaylists", s.GetPlaylists)
	bind("/rest/getPlaylist", s.GetPlaylist)
	bind("/rest/createPlaylist", s.CreatePlaylist)
	bind("/rest/updatePlaylist", s.UpdatePlaylist)
	bind("/rest/deletePlaylist", s.DeletePlaylist)

	// Media annotation / playback state.
	bind("/rest/star", s.Star)
	bind("/rest/unstar", s.Unstar)
	bind("/rest/setRating", s.SetRating)
	bind("/rest/getStarred", s.GetStarred)
	bind("/rest/getStarred2", s.GetStarred2)
	bind("/rest/scrobble", s.Scrobble)
	bind("/rest/getNowPlaying", s.GetNowPlaying)
	bind("/rest/getPlayQueue", s.GetPlayQueue)
	bind("/rest/savePlayQueue", s.SavePlayQueue)

	// Scanner + genres.
	bind("/rest/getScanStatus", s.GetScanStatus)
	bind("/rest/getScanStatus2", s.GetScanStatus)
	bind("/rest/startScan", s.StartScan)
	bind("/rest/getGenres", s.Genres)
	bind("/rest/getSongsByGenre", s.SongsByGenre)
	bind("/rest/getTopSongs", s.TopSongs)
	bind("/rest/getSimilarSongs", s.SimilarSongs)
	bind("/rest/getSimilarSongs2", s.SimilarSongs2)

	// Empty-but-valid reads for stores this server does not implement.
	bind("/rest/getShares", s.EmptyShares)
	bind("/rest/refreshShares", s.EmptyShares)
	bind("/rest/getBookmarks", s.EmptyBookmarks)
	bind("/rest/getInternetRadioStations", s.EmptyRadioStations)
	bind("/rest/getVideos", s.EmptyVideos)
	bind("/rest/getPodcasts", s.EmptyPodcasts)
	bind("/rest/getNewestPodcasts", s.EmptyPodcasts)
	bind("/rest/getChatMessages", s.EmptyChat)
	bind("/rest/getCaptions", s.NotImplemented)

	// Mutating endpoints for those stores: answer with a Subsonic error
	// rather than 404 so clients report "not supported", not "server down".
	for _, path := range []string{
		"/rest/createShare", "/rest/updateShare", "/rest/deleteShare",
		"/rest/createBookmark", "/rest/deleteBookmark",
		"/rest/createInternetRadioStation", "/rest/updateInternetRadioStation",
		"/rest/deleteInternetRadioStation",
		"/rest/addChatMessage",
		"/rest/refreshPodcasts", "/rest/createPodcastChannel",
		"/rest/deletePodcastChannel", "/rest/deletePodcastEpisode",
		"/rest/downloadPodcastEpisode",
	} {
		bind(path, s.NotImplemented)
	}

	bind("/rest/jukeboxControl", s.Jukebox)
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
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With, X-ND-Authorization, X-ND-Client-Unique-Id, X-ND-Version, Range")
		w.Header().Set("Access-Control-Expose-Headers", "X-Total-Count, Content-Range, Content-Length, Accept-Ranges")
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
