package debrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// FindInCloud scans the user's existing magnets for one whose container
// name loosely matches artist/album, and returns the best audio file
// inside it. No upload, no waiting — Tier 2 of the pipeline.
// File trees come from a single batched /magnet/files call over the
// name-matching Ready magnets (v4.1 status carries no tree).
func (c *Client) FindInCloud(ctx context.Context, artist, album, title string) (magnet Magnet, file MagnetFile, ok bool) {
	magnets, err := c.ListMagnets(ctx)
	if err != nil || len(magnets) == 0 {
		return Magnet{}, MagnetFile{}, false
	}
	wantA := norm(artist)
	wantB := norm(album)

	type cand struct {
		mg    Magnet
		score int
	}
	var cands []cand
	for _, mg := range magnets {
		// Only completed torrents can be unlocked.
		if mg.Status != StatusReady {
			continue
		}
		name := norm(mg.Filename)
		score := 0
		if wantA != "" && strings.Contains(name, wantA) {
			score += 2
		}
		if wantB != "" && strings.Contains(name, wantB) {
			score += 3
		}
		if score == 0 {
			continue
		}
		cands = append(cands, cand{mg, score})
	}
	if len(cands) == 0 {
		return Magnet{}, MagnetFile{}, false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	ids := make([]int64, 0, len(cands))
	for _, cd := range cands {
		ids = append(ids, cd.mg.ID)
	}
	trees, err := c.MagnetFiles(ctx, ids...)
	if err != nil {
		return Magnet{}, MagnetFile{}, false
	}
	for _, cd := range cands {
		if f, good := PickAudioFile(trees[cd.mg.ID], title); good {
			return cd.mg, f, true
		}
	}
	return Magnet{}, MagnetFile{}, false
}

// PreferAlbumTorrents stable-sorts candidates so torrents whose display
// name contains the album come first. Whole-album releases are better
// seeded and better quality than single-track rips, and one upload makes
// every track on the album instantly playable via the cloud tier.
// Candidates without a parseable name (opaque index download URLs) keep
// their relative order after the album matches.
func PreferAlbumTorrents(cands []string, album string) []string {
	want := norm(album)
	if want == "" || len(cands) < 2 {
		return cands
	}
	var match, rest []string
	for _, c := range cands {
		if dn := magnetName(c); dn != "" && strings.Contains(norm(dn), want) {
			match = append(match, c)
			continue
		}
		rest = append(rest, c)
	}
	if len(match) == 0 {
		return cands
	}
	return append(match, rest...)
}

// magnetName extracts the dn= display name from a magnet URI (""
// when absent or undecodable).
func magnetName(magnet string) string {
	i := strings.Index(magnet, "dn=")
	if i < 0 {
		return ""
	}
	v := magnet[i+3:]
	if j := strings.IndexByte(v, '&'); j >= 0 {
		v = v[:j]
	}
	if dec, err := url.QueryUnescape(v); err == nil {
		return dec
	}
	return strings.ReplaceAll(v, "+", " ")
}

// PickAudioFile returns the best .flac/.mp3/.m4a/.ogg/.opus/.wav match for
// title inside an already-Ready magnet. Exported for handlers/tests.
func PickAudioFile(files []MagnetFile, title string) (MagnetFile, bool) {
	f, s := bestAudioFile(files, title)
	if s < 0 {
		return MagnetFile{}, false
	}
	return f, true
}

func bestAudioFile(files []MagnetFile, title string) (MagnetFile, int) {
	want := norm(title)
	best := -1
	var out MagnetFile
	for _, f := range files {
		low := strings.ToLower(f.Filename)
		if !isAudio(low) {
			continue
		}
		score := 0
		n := norm(f.Filename)
		if want != "" && strings.Contains(n, want) {
			score += 5
		}
		if strings.HasSuffix(low, ".flac") {
			score += 3 // prefer lossless
		} else if strings.HasSuffix(low, ".mp3") {
			score += 1
		}
		// Prefer larger files (full track over sample/snippet).
		score += int(min64(f.Size/(10<<20), 3))
		if score > best {
			best = score
			out = f
		}
	}
	return out, best
}

func isAudio(name string) bool {
	for _, ext := range []string{".flac", ".mp3", ".m4a", ".ogg", ".opus", ".wav", ".aac", ".wma"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// norm lowercases, strips common separators and diacritic-adjacent noise
// for fuzzy container/track matching.
func norm(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer("_", " ", ".", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ")
	s = r.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------------------
// Prowlarr indexer search (Tier 3 source of magnets)
// ---------------------------------------------------------------------------

// Prowlarr queries a Prowlarr/Sonarr-style instance for torrent magnets.
// Empty baseURL disables it (returns no results, nil error) so the server
// runs fine without an indexer configured.
type Prowlarr struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// MinSeeders drops index rows reporting fewer seeders (default 0 =
	// no filtering). Index seeder counts skew optimistic, so this is a
	// coarse pre-filter only; AllDebrid-side stall detection is the
	// backstop for dead swarms. Note: already-cached AllDebrid torrents
	// play instantly regardless, so aggressive floors can only hide
	// instantly-playable results when the index under-reports.
	MinSeeders int
}

// NewProwlarr builds an indexer client; pass "" to disable.
func NewProwlarr(baseURL, apiKey string, httpClient *http.Client) *Prowlarr {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Prowlarr{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// Enabled reports whether indexer search is configured.
func (p *Prowlarr) Enabled() bool { return p != nil && p.baseURL != "" && p.apiKey != "" }

// Search returns up to limit magnet links (or .torrent URLs as fallback)
// ordered by seeders desc, for e.g. "artist album FLAC".
func (p *Prowlarr) Search(ctx context.Context, query string, limit int) ([]string, error) {
	if !p.Enabled() {
		return nil, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	q := url.Values{}
	q.Set("query", query)
	q.Set("type", "search")
	u := p.baseURL + "/api/v1/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("prowlarr: HTTP 401 unauthorized — check PROWLARR_API_KEY")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prowlarr: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Title    string `json:"title"`
		Magnet   string `json:"magnetUrl"`
		InfoURL  string `json:"infoUrl"`
		Download string `json:"downloadUrl"`
		Seeders  int    `json:"seeders"`
		Size     int64  `json:"size"`
		Indexer  string `json:"indexer"`
		Protocol string `json:"protocol"` // "torrent" | "usenet"
	}
	// Prowlarr returns a bare JSON array.
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("prowlarr: decode: %w", err)
	}
	var out []string
	skippedUsenet, skippedSeed := 0, 0
	for len(out) < limit {
		maxIdx := -1
		for i, r := range rows {
			// AllDebrid's upload API accepts BitTorrent magnets/torrents
			// only — Usenet (NZB) results can never resolve, so skip them
			// instead of burning upload/Ready-wait cycles on them.
			if p := strings.ToLower(r.Protocol); p != "" && p != "torrent" {
				continue
			}
			if r.Seeders < p.MinSeeders {
				continue
			}
			if r.Magnet == "" && r.Download == "" {
				continue
			}
			used := false
			for _, o := range out {
				if o == r.Magnet || (r.Magnet == "" && o == r.Download) {
					used = true
					break
				}
			}
			if used {
				continue
			}
			if maxIdx == -1 || rows[maxIdx].Seeders < r.Seeders {
				maxIdx = i
			}
		}
		if maxIdx == -1 {
			break
		}
		if rows[maxIdx].Magnet != "" {
			out = append(out, rows[maxIdx].Magnet)
		} else {
			out = append(out, rows[maxIdx].Download)
		}
	}
	for _, r := range rows {
		if proto := strings.ToLower(r.Protocol); proto != "" && proto != "torrent" {
			skippedUsenet++
		} else if r.Seeders < p.MinSeeders {
			skippedSeed++
		}
	}
	log.Printf("prowlarr: query=%q rows=%d usable=%d skipped_usenet=%d skipped_minseed=%d",
		query, len(rows), len(out), skippedUsenet, skippedSeed)
	return out, nil
}

// FetchDownload resolves an indexer's .torrent link. Prowlarr serves these
// itself (LAN-only, API-key gated), so the key is attached when the URL
// points at our own indexer host. Redirects are NOT followed blindly: for
// magnet-only results Prowlarr 302-redirects to the magnet: URI, which is
// captured and returned for /magnet/upload instead of choking an HTTP
// client with "unsupported protocol scheme". A single http(s) redirect hop
// is followed manually (key re-attached); anything else is an error.
// Raw bytes are sanity-checked as bencoded torrent data ('d' + "4:info")
// to catch HTML error pages before they reach AllDebrid.
func (p *Prowlarr) FetchDownload(ctx context.Context, rawURL string) (FetchedTorrent, error) {
	client := p.http
	if client == nil {
		client = http.DefaultClient
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	fetch := func(target string) (*http.Response, error) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if u, e := url.Parse(target); e == nil && p.baseURL != "" {
			if b, e2 := url.Parse(p.baseURL); e2 == nil &&
				strings.EqualFold(u.Host, b.Host) && u.Host != "" {
				req.Header.Set("X-Api-Key", p.apiKey)
			}
		}
		req.Header.Set("User-Agent", "DebridSubsonic/1.0")
		return noRedirect.Do(req)
	}

	resp, err := fetch(rawURL)
	if err != nil {
		return FetchedTorrent{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusMovedPermanently ||
		resp.StatusCode == http.StatusFound ||
		resp.StatusCode == http.StatusSeeOther ||
		resp.StatusCode == http.StatusTemporaryRedirect ||
		resp.StatusCode == http.StatusPermanentRedirect {
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if strings.HasPrefix(strings.ToLower(loc), "magnet:") {
			return FetchedTorrent{Magnet: loc}, nil
		}
		ref, err := url.Parse(loc)
		if err != nil {
			return FetchedTorrent{}, fmt.Errorf("prowlarr download: bad redirect %q", loc)
		}
		abs := resp.Request.URL.ResolveReference(ref).String()
		if strings.HasPrefix(strings.ToLower(abs), "http://") ||
			strings.HasPrefix(strings.ToLower(abs), "https://") {
			_ = resp.Body.Close()
			resp2, err := fetch(abs)
			if err != nil {
				return FetchedTorrent{}, err
			}
			defer resp2.Body.Close()
			resp = resp2
		} else {
			return FetchedTorrent{}, fmt.Errorf("prowlarr download: redirect to %q", loc)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return FetchedTorrent{}, fmt.Errorf("prowlarr download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return FetchedTorrent{}, err
	}
	if len(data) == 0 || data[0] != 'd' || !strings.Contains(string(data[:min(512, len(data))]), "4:info") {
		return FetchedTorrent{}, errors.New("prowlarr download: not a torrent file")
	}
	name := "download.torrent"
	if u, e := url.Parse(rawURL); e == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			name = base
		}
	}
	return FetchedTorrent{Filename: name, Data: data}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// EnsureSeededUpload tries each candidate magnet in order until one reaches
// Ready state, returning the Ready magnet with its file tree populated.
// Candidates usually come from Prowlarr.Search or PublicIndex.Search.
// Stage failures are wrapped distinctly (upload vs wait-ready vs files) so
// stream logs pinpoint the failing AllDebrid call.
func (c *Client) EnsureSeededUpload(ctx context.Context, candidates []string, waitPer time.Duration) (Magnet, error) {
	if len(candidates) == 0 {
		return Magnet{}, errors.New("alldebrid: no candidates to upload")
	}
	if waitPer <= 0 {
		waitPer = 90 * time.Second
	}
	var lastErr error
	for _, cand := range candidates {
		id, err := c.uploadOne(ctx, cand)
		if err != nil {
			lastErr = fmt.Errorf("upload: %w", err)
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, waitPer)
		m, err := c.WaitReady(wctx, id, 4*time.Second)
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("wait-ready (magnet %d): %w", id, err)
			continue
		}
		trees, err := c.MagnetFiles(ctx, m.ID)
		if err != nil {
			lastErr = fmt.Errorf("files (magnet %d): %w", m.ID, err)
			continue
		}
		m.Links = trees[m.ID]
		return m, nil
	}
	if lastErr == nil {
		lastErr = errors.New("alldebrid: all uploads failed")
	}
	return Magnet{}, lastErr
}
