package soulseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Audio extensions eligible for download.
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true,
	".opus": true, ".wav": true, ".aac": true, ".wma": true,
}

// Tunables for the interactive play path.
const (
	searchPollEvery = 2 * time.Second
	searchTimeout   = 40 * time.Second
	xferPollEvery   = 5 * time.Second
	// Download budget per track. Soulseek peers are slow; the request
	// context (client disconnect) ends the wait earlier, while slskd
	// keeps downloading server-side so a retry converges instantly.
	downloadTimeout = 10 * time.Minute
)

// DefaultDownloadDir is slskd's own container default for completed
// downloads (/app/downloads in the official image). It is assumed when
// SLSKD_DOWNLOAD_DIR is unset, so the compose stack works with zero path
// config as long as the downloads volume is mounted there (see
// docker-compose.yml). A custom slskd downloads dir still needs the env var.
const DefaultDownloadDir = "/app/downloads"

// Client drives a self-hosted slskd instance (https://slskd.org) over its
// /api/v0 REST API: search the Soulseek network, queue the best match,
// wait for completion, resolve the local file.
//
// slskd exposes listing/deletion of its download dir over the API but no
// file *content* endpoint, so completed files are served from a shared
// filesystem (same host bind mount or shared volume). Point
// SLSKD_DOWNLOAD_DIR at slskd's downloads folder as seen locally.
type Client struct {
	baseURL string
	apiKey  string
	dlDir   string
	http    *http.Client

	// flying coalesces concurrent fetches for the same track (Feishin
	// retries every few seconds while waiting): one leader searches and
	// downloads, followers wait on its result and play the same file the
	// moment it completes — never a duplicate or a different pick.
	fmu    sync.Mutex
	flying map[string]*inflight
}

type inflight struct {
	done chan struct{}
	path string
	err  error
}

// New builds a client. An empty downloadDir falls back to
// DefaultDownloadDir.
func New(baseURL, apiKey, downloadDir string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if strings.TrimSpace(downloadDir) == "" {
		downloadDir = DefaultDownloadDir
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		dlDir:   downloadDir,
		http:    httpClient,
		flying:  make(map[string]*inflight),
	}
}

// Enabled reports whether Soulseek fetching is configured.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// DownloadDir returns the resolved local download directory.
func (c *Client) DownloadDir() string {
	if c == nil {
		return ""
	}
	return c.dlDir
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("slskd: HTTP 401 unauthorized — check SLSKD_API_KEY")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("slskd: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("slskd: decode %s %s: %w", method, path, err)
	}
	return nil
}

// Candidate is one downloadable network file.
type Candidate struct {
	Username string
	Filename string
	Size     int64
	Speed    int64
	FreeSlot bool
}

type searchObj struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type searchResponse struct {
	Username          string `json:"username"`
	HasFreeUploadSlot bool   `json:"hasFreeUploadSlot"`
	UploadSpeed       int64  `json:"uploadSpeed"`
	QueueLength       int64  `json:"queueLength"`
	Files             []struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"files"`
}

// search runs one network search to completion and returns ranked candidates.
func (c *Client) search(ctx context.Context, query string, limit int) ([]Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	var so searchObj
	if err := c.do(ctx, http.MethodPost, "/api/v0/searches", map[string]any{
		"searchText": query,
		// Ask slskd to cap the result set server-side where supported.
		"responseLimit": limit * 10,
		"searchTimeout": 15000,
	}, &so); err != nil {
		return nil, err
	}
	if so.ID == "" {
		return nil, errors.New("slskd: search returned no id")
	}
	defer func() {
		_ = c.do(context.WithoutCancel(ctx), http.MethodDelete, "/api/v0/searches/"+so.ID, nil, nil)
	}()
	// Poll to terminal state.
	t := time.NewTicker(searchPollEvery)
	defer t.Stop()
	for {
		var cur searchObj
		if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+so.ID, nil, &cur); err != nil {
			return nil, err
		}
		if !strings.EqualFold(cur.State, "InProgress") {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	var resps []searchResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+so.ID+"/responses", nil, &resps); err != nil {
		return nil, err
	}
	var out []Candidate
	for _, r := range resps {
		for _, f := range r.Files {
			if !audioExts[strings.ToLower(filepath.Ext(f.Filename))] {
				continue
			}
			out = append(out, Candidate{
				Username: r.Username, Filename: f.Filename, Size: f.Size,
				Speed: r.UploadSpeed, FreeSlot: r.HasFreeUploadSlot,
			})
		}
	}
	log.Printf("soulseek: query=%q responses=%d audio=%d", query, len(resps), len(out))
	return out, nil
}

// score ranks candidates for a track: the core title (edition tags like
// "(Studio Recording ...)" stripped — peers never include those in
// filenames) in filename required, album containment and lossless format
// rank, free slots and speed break ties.
func score(cands []Candidate, album, title string) []Candidate {
	titleCore := stripBrackets(title)
	if titleCore == "" {
		titleCore = title // degenerate title made only of brackets
	}
	wantT, wantB := norm(titleCore), norm(stripBrackets(album))
	type scored struct {
		c Candidate
		s int
	}
	var ranked []scored
	for _, cd := range cands {
		n := norm(cd.Filename)
		if wantT == "" || !strings.Contains(n, wantT) {
			continue
		}
		s := 10
		if wantB != "" && strings.Contains(n, wantB) {
			s += 10
		}
		if strings.HasSuffix(strings.ToLower(cd.Filename), ".flac") {
			s += 5
		}
		if cd.FreeSlot {
			s += 2
		}
		s += int(min(cd.Speed/(100<<10), 3)) // up to +3 for fast peers
		ranked = append(ranked, scored{cd, s})
	}
	// Selection sort by score desc (n is small).
	var out []Candidate
	for len(ranked) > 0 {
		best := 0
		for i := range ranked {
			if ranked[i].s > ranked[best].s {
				best = i
			}
		}
		out = append(out, ranked[best].c)
		ranked = append(ranked[:best], ranked[best+1:]...)
	}
	return out
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// transferFile is one file inside slskd's transfer view (subset of fields).
type transferFile struct {
	Username  string `json:"username"`
	Directory string `json:"directory"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	State     string `json:"state"`
}

// listDownloads normalizes slskd's transfer view, which groups files by
// user/directory, into a flat file list. A flat array is accepted too.
func (c *Client) listDownloads(ctx context.Context, username string) ([]transferFile, error) {
	var grouped []struct {
		Username    string `json:"username"`
		Directories []struct {
			Files []transferFile `json:"files"`
		} `json:"directories"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v0/transfers/downloads/"+username, nil, &grouped)
	if err == nil {
		var out []transferFile
		for _, g := range grouped {
			for _, d := range g.Directories {
				out = append(out, d.Files...)
			}
		}
		return out, nil
	}
	var flat []transferFile
	if ferr := c.do(ctx, http.MethodGet, "/api/v0/transfers/downloads/"+username, nil, &flat); ferr != nil {
		return nil, err // original grouped error is more informative
	}
	return flat, nil
}

func terminalOK(state string) bool { return strings.EqualFold(state, "Completed") }

// errCandidateUnusable marks a failure worth trying the next peer for
// (enqueue rejected, transfer terminally failed). Trivial conditions —
// a context deadline or a transfer still in progress — must NOT hop:
// slskd keeps downloading server-side, so a retry converges on the same
// file rather than queueing a different one.
var errCandidateUnusable = errors.New("candidate unusable")

func terminalBad(state string) bool {
	switch {
	case strings.EqualFold(state, "Failed"),
		strings.EqualFold(state, "Rejected"),
		strings.EqualFold(state, "TimedOut"),
		strings.EqualFold(state, "Cancelled"),
		strings.EqualFold(state, "Canceled"):
		return true
	}
	return false
}

// FetchTrackShared is the coalesced entry point handlers call. Concurrent
// requests for the same track collapse onto one leader: the leader runs
// FetchTrack detached from any single client connection (so one retry
// timing out can't kill everyone else's wait), followers block until it
// completes and then play the very same file. Late arrivals redo the
// lookup, which converges instantly on the completed download.
func (c *Client) FetchTrackShared(ctx context.Context, artist, album, title string) (string, error) {
	key := artist + "\x00" + album + "\x00" + title
	c.fmu.Lock()
	if f, ok := c.flying[key]; ok {
		c.fmu.Unlock()
		log.Printf("soulseek: %q - %q: joining in-flight fetch", artist, title)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
			return f.path, f.err
		}
	}
	f := &inflight{done: make(chan struct{})}
	c.flying[key] = f
	c.fmu.Unlock()

	log.Printf("soulseek: %q - %q: leading fetch", artist, title)
	f.path, f.err = c.FetchTrack(context.WithoutCancel(ctx), artist, album, title)
	close(f.done)
	c.fmu.Lock()
	delete(c.flying, key)
	c.fmu.Unlock()
	return f.path, f.err
}

// FetchTrack searches Soulseek for a track, downloads the best match via
// slskd, and returns the local path of the completed file. Repeat calls
// converge: an already-completed download is served without re-queueing.
// The download dir must be visible locally (shared volume/bind mount);
// otherwise this fails fast instead of waiting out a download whose bytes
// could never be served.
func (c *Client) FetchTrack(ctx context.Context, artist, album, title string) (string, error) {
	if fi, err := os.Stat(c.dlDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("soulseek download dir %s not accessible "+
			"(mount slskd's downloads folder there, or set SLSKD_DOWNLOAD_DIR)", c.dlDir)
	}
	queries := []string{
		strings.TrimSpace(strings.Join(strings.Fields(artist+" "+album), " ")),
		strings.TrimSpace(strings.Join(strings.Fields(artist+" "+title), " ")),
	}
	var cands []Candidate
	for _, q := range queries {
		if q == "" {
			continue
		}
		got, err := c.search(ctx, q, 10)
		if err != nil {
			log.Printf("soulseek: search %q: %v", q, err)
			continue
		}
		if ranked := score(got, album, title); len(ranked) > 0 {
			cands = ranked
			log.Printf("soulseek: query=%q picked %d candidate(s)", q, len(ranked))
			break
		}
	}
	if len(cands) == 0 {
		return "", errors.New("soulseek: no results")
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	var lastErr error
	for i, cd := range cands {
		if i >= 3 {
			break // bound peer-hopping per stream
		}
		path, err := c.fetchOne(dctx, cd)
		if err == nil {
			return path, nil
		}
		log.Printf("soulseek: %s@%s: %v", cd.Filename, cd.Username, err)
		if !errors.Is(err, errCandidateUnusable) {
			// Stick with the pick already being downloaded server-side:
			// hopping would queue a second, different file. A retry
			// converges on the same one instead.
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("soulseek: all candidates failed")
	}
	return "", lastErr
}

// fetchOne ensures one candidate is downloaded and resolves its local path.
// A completed earlier attempt serves instantly; a still-running one is
// waited on, never re-enqueued (re-queueing the same file just duplicates
// queue entries, and concurrent retries must not each pick anew).
func (c *Client) fetchOne(ctx context.Context, cd Candidate) (string, error) {
	needEnqueue := true
	if files, err := c.listDownloads(ctx, cd.Username); err == nil {
		for _, f := range files {
			if !sameTrack(f.Filename, cd.Filename) {
				continue
			}
			switch {
			case terminalOK(f.State):
				if p, ok := c.resolve(cd.Filename); ok {
					return p, nil
				}
			case terminalBad(f.State):
				// Stale failure: fall through and try enqueueing again.
			default:
				log.Printf("soulseek: %s already %s, waiting on it", cd.Filename, f.State)
				needEnqueue = false
			}
		}
	}
	if needEnqueue {
		if err := c.do(ctx, http.MethodPost, "/api/v0/transfers/downloads/"+cd.Username,
			[]map[string]any{{"filename": cd.Filename, "size": cd.Size}}, nil); err != nil {
			return "", fmt.Errorf("%w: enqueue: %v", errCandidateUnusable, err)
		}
	}
	t := time.NewTicker(xferPollEvery)
	defer t.Stop()
	for {
		files, err := c.listDownloads(ctx, cd.Username)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			if !sameTrack(f.Filename, cd.Filename) {
				continue
			}
			switch {
			case terminalOK(f.State):
				if p, ok := c.resolve(cd.Filename); ok {
					return p, nil
				}
				return "", fmt.Errorf("soulseek: %s completed but not found under %s [%s]",
					filepath.Base(cd.Filename), c.dlDir, c.dirSample())
			case terminalBad(f.State):
				return "", fmt.Errorf("%w: transfer %s", errCandidateUnusable, f.State)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// resolve maps a Soulseek network filename to a file under the download
// dir. slskd lays downloads out per-user with sanitized names, so instead
// of reconstructing its layout, scan for the basename with a loose
// alphanumeric comparison. The username subdir is preferred when present.
func (c *Client) resolve(networkName string) (string, bool) {
	base := networkName
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	want := alnum(strings.ToLower(base))
	if want == "" {
		return "", false
	}
	match := func(name string) bool {
		got := alnum(strings.ToLower(name))
		return got == want || strings.Contains(got, want) || strings.Contains(want, got)
	}
	// Prefer per-user subdirs (slskd default layout), then root-level files.
	if entries, err := os.ReadDir(c.dlDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			var found string
			_ = filepath.WalkDir(filepath.Join(c.dlDir, e.Name()), func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || found != "" {
					return nil
				}
				if audioExts[strings.ToLower(filepath.Ext(p))] && match(filepath.Base(p)) {
					found = p
				}
				return nil
			})
			if found != "" {
				return found, true
			}
		}
		for _, e := range entries {
			if !e.IsDir() && audioExts[strings.ToLower(filepath.Ext(e.Name()))] && match(e.Name()) {
				return filepath.Join(c.dlDir, e.Name()), true
			}
		}
		return "", false
	}
	// Download dir itself unreadable as a listing: deep scan attempt.
	var found string
	_ = filepath.WalkDir(c.dlDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		if audioExts[strings.ToLower(filepath.Ext(p))] && match(filepath.Base(p)) {
			found = p
		}
		return nil
	})
	if found != "" {
		return found, true
	}
	return "", false
}

// sameTrack matches transfer entries to candidates across slskd's path
// normalization (separators, case).
func sameTrack(a, b string) bool {
	ab, bb := a, b
	if i := strings.LastIndexAny(ab, `/\`); i >= 0 {
		ab = ab[i+1:]
	}
	if i := strings.LastIndexAny(bb, `/\`); i >= 0 {
		bb = bb[i+1:]
	}
	return strings.EqualFold(strings.TrimSpace(ab), strings.TrimSpace(bb))
}

// dirSample lists a few files under the download dir for diagnostics when
// a completed transfer cannot be found on disk (usually a mount mismatch:
// SLSKD_DOWNLOAD_DIR must point at slskd's own completed-downloads folder).
func (c *Client) dirSample() string {
	var names []string
	_ = filepath.WalkDir(c.dlDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		names = append(names, filepath.Base(p))
		if len(names) >= 5 {
			return fs.SkipAll
		}
		return nil
	})
	if len(names) == 0 {
		return "no files"
	}
	return strings.Join(names, ", ")
}

func alnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func norm(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer("_", " ", ".", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ")
	s = r.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// stripBrackets removes (...) and [...] groups (edition tags, live notes)
// that metadata carries but filenames and index torrents omit.
func stripBrackets(s string) string {
	for {
		start := strings.IndexAny(s, "([")
		if start == -1 {
			break
		}
		closer := ")"
		if s[start] == '[' {
			closer = "]"
		}
		rest := s[start+1:]
		end := strings.Index(rest, closer)
		if end == -1 {
			break
		}
		s = s[:start] + " " + rest[end+1:]
	}
	return strings.Join(strings.Fields(s), " ")
}
