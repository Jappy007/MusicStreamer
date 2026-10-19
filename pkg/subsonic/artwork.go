package subsonic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/metadata"
	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
)

const (
	maxCoverBytes     = 8 << 20
	coverFailureTTL   = time.Hour
	coverTransientTTL = 30 * time.Second
)

func validCoverKey(key string) bool {
	if key == "" || len(key) > 256 {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, id string) {
	key := strings.TrimSpace(id)
	if !validCoverKey(key) {
		writeCoverError(w, http.StatusNotFound)
		return
	}
	if s.artwork == nil {
		writeCoverError(w, http.StatusServiceUnavailable)
		return
	}
	if path, ok := s.artwork.Lookup(key); ok {
		s.serveCoverFile(w, r, path)
		return
	}

	s.coverMu.Lock()
	if f, ok := s.coverFail[key]; ok {
		if time.Now().Before(f.exp) {
			status := f.status
			s.coverMu.Unlock()
			writeCoverError(w, status)
			return
		}
		delete(s.coverFail, key)
	}
	if job, ok := s.coverJobs[key]; ok {
		s.coverMu.Unlock()
		select {
		case <-job.done:
			if path, ok := s.artwork.Lookup(key); ok {
				s.serveCoverFile(w, r, path)
				return
			}
			status := job.status
			if status == 0 {
				status = http.StatusServiceUnavailable
			}
			writeCoverError(w, status)
		case <-r.Context().Done():
			return
		case <-time.After(40 * time.Second):
			writeCoverError(w, http.StatusServiceUnavailable)
		}
		return
	}
	if s.coverJobs == nil {
		s.coverJobs = make(map[string]*coverJob)
	}
	job := &coverJob{done: make(chan struct{})}
	s.coverJobs[key] = job
	s.coverMu.Unlock()

	status := http.StatusServiceUnavailable
	if s.coverSem != nil {
		select {
		case s.coverSem <- struct{}{}:
			defer func() { <-s.coverSem }()
		case <-r.Context().Done():
			s.finishCover(key, status)
			return
		case <-time.After(10 * time.Second):
			s.finishCover(key, status)
			writeCoverError(w, status)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	status = s.resolveCover(ctx, key)
	cancel()
	s.finishCover(key, status)

	if path, ok := s.artwork.Lookup(key); ok && status == http.StatusOK {
		s.serveCoverFile(w, r, path)
		return
	}
	writeCoverError(w, status)
}

func (s *Server) finishCover(key string, status int) {
	if status == http.StatusOK {
		if _, ok := s.artwork.Lookup(key); !ok {
			status = http.StatusServiceUnavailable
		}
	}
	s.coverMu.Lock()
	if job, ok := s.coverJobs[key]; ok {
		job.status = status
		delete(s.coverJobs, key)
		close(job.done)
	}
	if status == http.StatusNotFound {
		s.setCoverFailure(key, http.StatusNotFound, coverFailureTTL)
	}
	if status == http.StatusServiceUnavailable {
		s.setCoverFailure(key, http.StatusServiceUnavailable, coverTransientTTL)
	}
	if status == http.StatusOK {
		delete(s.coverFail, key)
	}
	s.coverMu.Unlock()
}

func (s *Server) setCoverFailure(key string, status int, ttl time.Duration) {
	if s.coverFail == nil {
		s.coverFail = make(map[string]coverFailure)
	}
	now := time.Now()
	for k, f := range s.coverFail {
		if now.After(f.exp) {
			delete(s.coverFail, k)
		}
	}
	s.coverFail[key] = coverFailure{status: status, exp: now.Add(ttl)}
}

func (s *Server) resolveCover(ctx context.Context, key string) int {
	artist, album, title, err := metadata.SplitID(key)
	if err != nil || (artist == "" && album == "" && title == "") {
		return http.StatusNotFound
	}
	cand := cachedSong{artist: artist, album: album, title: title}
	if title == "" {
		if cs, ok := s.firstCachedSong(artist, album); ok {
			cand = cs
		}
	}
	albumKey := metadata.EncodeID(cand.artist, cand.album, "")
	isArtist := title == "" && album == ""

	transient := false
	try := func(u string) bool {
		if strings.TrimSpace(u) == "" {
			return false
		}
		ok, tr := s.fetchAndStoreCover(ctx, key, u)
		if tr {
			transient = true
		}
		return ok
	}

	if u := s.thumbnailFor(key); try(u) {
		return http.StatusOK
	}
	if albumKey != key {
		if u := s.thumbnailFor(albumKey); try(u) {
			return http.StatusOK
		}
	}

	var songs []metadata.Song
	if s.meta != nil {
		query := strings.Join(strings.Fields(artist+" "+album), " ")
		if isArtist {
			query = artist
		}
		if strings.TrimSpace(query) == "" {
			query = cand.title
		}
		if query != "" {
			got, serr := s.meta.Search(ctx, query, 8)
			if serr != nil {
				transient = true
			} else {
				songs = got
				for _, m := range songs {
					if m.CoverURL != "" && try(m.CoverURL) {
						return http.StatusOK
					}
				}
			}
		}
	}

	ytArtist, ytTitle := artist, cand.title
	if ytTitle == "" && len(songs) > 0 {
		ytTitle = songs[0].Title
		if ytArtist == "" {
			ytArtist = songs[0].Artist
		}
	}
	if s.yt != nil && s.yt.Enabled() && strings.TrimSpace(ytTitle) != "" {
		st, rerr := s.yt.Resolve(ctx, ytArtist, ytTitle, false)
		if rerr != nil {
			transient = true
		} else {
			s.rememberThumbnail(key, st.ThumbnailURL)
			if try(st.ThumbnailURL) {
				return http.StatusOK
			}
			if st.VideoID != "" {
				if alt := ytaudio.ThumbnailURLForVideo(st.VideoID); alt != st.ThumbnailURL && try(alt) {
					return http.StatusOK
				}
			}
		}
	}

	if transient {
		return http.StatusServiceUnavailable
	}
	return http.StatusNotFound
}

func (s *Server) firstCachedSong(artist, album string) (cachedSong, bool) {
	cat := s.catalog()
	if album != "" {
		if g := cat.byArtist[artist][album]; len(g) > 0 {
			return g[0], true
		}
	}
	for _, cs := range cat.songs {
		if cs.artist == artist {
			return cs, true
		}
	}
	return cachedSong{}, false
}

func (s *Server) fetchAndStoreCover(ctx context.Context, key, rawURL string) (bool, bool) {
	data, tr := s.fetchImage(ctx, rawURL)
	if data == nil {
		return false, tr
	}
	if s.artwork == nil {
		return false, true
	}
	if err := s.artwork.Store(key, bytes.NewReader(data)); err != nil {
		log.Printf("cover: cache store %s: %v", key, err)
		return false, true
	}
	s.coverMu.Lock()
	delete(s.coverFail, key)
	s.coverMu.Unlock()
	return true, false
}

func (s *Server) fetchImage(ctx context.Context, rawURL string) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", "MusicStreamer/1.0")
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,image/*;q=0.8")
	resp, err := s.proxy.Do(req)
	if err != nil {
		return nil, true
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, false
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true
	default:
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, true
	}
	if len(data) == 0 || len(data) > maxCoverBytes {
		return nil, true
	}
	ctype := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	sniff := http.DetectContentType(data)
	if !isRasterImage(ctype) {
		ctype = sniff
	}
	if !isRasterImage(ctype) {
		log.Printf("cover: rejecting non-image %s from %s", ctype, rawURL)
		return nil, false
	}
	return data, false
}

func isRasterImage(ctype string) bool {
	switch ctype {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

func (s *Server) serveCoverFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeCoverError(w, http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeCoverError(w, http.StatusNotFound)
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := http.DetectContentType(head[:n])
	if !isRasterImage(ctype) {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	_, _ = f.Seek(0, io.SeekStart)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func writeCoverError(w http.ResponseWriter, status int) {
	if status != http.StatusNotFound && status != http.StatusServiceUnavailable {
		status = http.StatusNotFound
	}
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "60")
	}
	http.Error(w, http.StatusText(status), status)
}

func (s *Server) rememberThumbnail(id, thumb string) {
	thumb = strings.TrimSpace(thumb)
	if thumb == "" || id == "" {
		return
	}
	s.coverMu.Lock()
	if s.coverThumbs == nil {
		s.coverThumbs = make(map[string]string)
	}
	s.coverThumbs[id] = thumb
	if artist, album, _, err := metadata.SplitID(id); err == nil && album != "" {
		albumID := metadata.EncodeID(artist, album, "")
		s.coverThumbs[albumID] = thumb
		delete(s.coverFail, albumID)
	}
	delete(s.coverFail, id)
	s.coverMu.Unlock()
}

func (s *Server) thumbnailFor(id string) string {
	s.coverMu.Lock()
	defer s.coverMu.Unlock()
	return s.coverThumbs[id]
}

func (s *Server) coverStats() string {
	if s.artwork == nil {
		return "disabled"
	}
	n, total, max := s.artwork.Stats()
	return fmt.Sprintf("%d entries, %.1fMiB/%.1fMiB", n, float64(total)/(1<<20), float64(max)/(1<<20))
}
