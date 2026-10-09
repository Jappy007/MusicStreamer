package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AllDebrid API endpoints. Magnet lifecycle moved to v4.1 on 16/10/2024:
// status no longer embeds the file tree (GET /v4/magnet/status is
// discontinued); file trees live at POST /v4/magnet/files.
// Docs: https://docs.alldebrid.com
const (
	apiBase   = "https://api.alldebrid.com/v4"
	apiBase41 = "https://api.alldebrid.com/v4.1"
)

// Magnet file status codes worth treating explicitly.
const (
	StatusReady       = "Ready"
	StatusDownloading = "Downloading"
	StatusProcessing  = "Processing"
)

// Client is a minimal, stdlib-only AllDebrid v4/v4.1 client with context
// timeouts on every request. Auth goes in the Authorization header
// (the legacy agent/apikey params were dropped from the documented flow).
type Client struct {
	apiKey string
	http   *http.Client
	base   string
	base41 string

	// TorrentFetch resolves an index download link (e.g. Prowlarr's, which
	// are LAN-only/key-gated and unreachable from AllDebrid's servers).
	// Nil means http candidates are skipped. Wire it to
	// Prowlarr.FetchDownload in main.
	TorrentFetch func(ctx context.Context, rawURL string) (FetchedTorrent, error)
}

// FetchedTorrent is the resolved form of an index download link: either
// raw .torrent bytes, or a magnet URI (Prowlarr 302-redirects magnet-only
// results to the magnet: link, which no HTTP client can GET).
type FetchedTorrent struct {
	Magnet   string
	Filename string
	Data     []byte
}

// New returns a Client. A nil httpClient gets a sane default with
// connection pooling tuned for streaming proxy workloads.
func New(apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &Client{apiKey: apiKey, http: httpClient, base: apiBase, base41: apiBase41}
}

// API envelope shared by all v4/v4.1 endpoints.
type envelope[T any] struct {
	Status string  `json:"status"`
	Data   T       `json:"data"`
	Error  *apiErr `json:"error"`
}

type apiErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiErr) Error() string {
	if e == nil {
		return "alldebrid: unknown error"
	}
	return "alldebrid: " + e.Code + ": " + e.Message
}

func isNotFound(err error) bool {
	var a *apiErr
	if errors.As(err, &a) {
		return a.Code == "404"
	}
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

// Magnet is a torrent/magnet entry in the user's cloud. Links is populated
// separately via MagnetFiles — v4.1 status responses carry no file tree.
// Seeder/activity fields mirror the latest status poll.
type Magnet struct {
	ID            int64        `json:"id"`
	Filename      string       `json:"filename"`
	Status        string       `json:"status"`
	Size          int64        `json:"size"`
	Seeders       int          `json:"seeders"`
	DownloadSpeed int64        `json:"downloadSpeed"`
	Downloaded    int64        `json:"downloaded"`
	Links         []MagnetFile `json:"links"`
}

// MagnetFile is one file inside a magnet's tree.
type MagnetFile struct {
	Link     string `json:"link"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// postForm POSTs a urlencoded form with Bearer auth and decodes the API
// envelope into out. Non-200 responses surface the decoded API error when
// the body carries one.
func (c *Client) postForm(ctx context.Context, base, path string, form url.Values, out any) error {
	if c.apiKey == "" {
		return errors.New("alldebrid: missing API key")
	}
	if form == nil {
		form = url.Values{}
	}
	form.Set("apikey", c.apiKey) // docs examples still send it alongside Bearer
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var env envelope[json.RawMessage]
		if jerr := json.Unmarshal(body, &env); jerr == nil && env.Status == "error" && env.Error != nil {
			return env.Error
		}
		return fmt.Errorf("alldebrid: POST %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("alldebrid: decode %s: %w", path, err)
	}
	return nil
}

// magnetStatus mirrors the v4.1 status object (no file tree).
type magnetStatus struct {
	ID            int64  `json:"id"`
	Filename      string `json:"filename"`
	Size          int64  `json:"size"`
	Status        string `json:"status"`
	StatusCode    int    `json:"statusCode"`
	Seeders       int    `json:"seeders"`
	DownloadSpeed int64  `json:"downloadSpeed"`
	Downloaded    int64  `json:"downloaded"`
}

// magnetList decodes data.magnets in whatever shape AllDebrid sends:
// an array for list queries, an object keyed by magnet ID, or the bare
// object for single-ID queries. Shapes are tried in order; the first one
// yielding at least one nonzero-ID entry wins.
type magnetList []magnetStatus

func (m *magnetList) UnmarshalJSON(b []byte) error {
	list, err := decodeFlexList[magnetStatus](b, func(s magnetStatus) bool {
		return s.ID != 0
	})
	if err != nil {
		return err
	}
	*m = list
	return nil
}

// decodeFlexList tolerates APIs that return a list as an array, as an
// object keyed by ID, or as a bare single object. valid reports whether a
// decoded item counts as real data (guards against swallowing a shape
// mismatch as an empty-but-valid result).
func decodeFlexList[T any](b []byte, valid func(T) bool) ([]T, error) {
	var arr []T
	if err := json.Unmarshal(b, &arr); err == nil {
		return arr, nil // [] included: unambiguous
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	if len(obj) > 0 {
		keyed := false
		for _, v := range obj {
			keyed = strings.HasPrefix(strings.TrimSpace(string(v)), "{")
			break
		}
		if keyed {
			out := make([]T, 0, len(obj))
			for _, raw := range obj {
				var item T
				if err := json.Unmarshal(raw, &item); err != nil {
					return nil, err
				}
				out = append(out, item)
			}
			for _, item := range out {
				if valid(item) {
					return out, nil
				}
			}
		}
	}
	var single T
	if err := json.Unmarshal(b, &single); err != nil {
		return nil, err
	}
	if valid(single) {
		return []T{single}, nil
	}
	return nil, fmt.Errorf("alldebrid: unrecognized list shape %.80s", strings.TrimSpace(string(b)))
}

func (c *Client) queryStatus(ctx context.Context, id *int64) ([]magnetStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{}
	if id != nil {
		form.Set("id", strconv.FormatInt(*id, 10))
	}
	var env envelope[struct {
		Magnets magnetList `json:"magnets"`
	}]
	if err := c.postForm(ctx, c.base41, "/magnet/status", form, &env); err != nil {
		return nil, err
	}
	if env.Status != "success" {
		return nil, env.Error
	}
	return []magnetStatus(env.Data.Magnets), nil
}

// ListMagnets returns all magnets in the account via v4.1 status.
func (c *Client) ListMagnets(ctx context.Context) ([]Magnet, error) {
	stats, err := c.queryStatus(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Magnet, 0, len(stats))
	for _, st := range stats {
		out = append(out, Magnet{
			ID: st.ID, Filename: st.Filename, Size: st.Size, Status: st.Status,
			Seeders: st.Seeders, DownloadSpeed: st.DownloadSpeed, Downloaded: st.Downloaded,
		})
	}
	return out, nil
}

// MagnetStatus fetches one magnet's live status via v4.1 (no file tree;
// follow up with MagnetFiles for links).
func (c *Client) MagnetStatus(ctx context.Context, id int64) (Magnet, error) {
	stats, err := c.queryStatus(ctx, &id)
	if err != nil {
		return Magnet{}, err
	}
	for _, st := range stats {
		if st.ID == id {
			return Magnet{
				ID: st.ID, Filename: st.Filename, Size: st.Size, Status: st.Status,
				Seeders: st.Seeders, DownloadSpeed: st.DownloadSpeed, Downloaded: st.Downloaded,
			}, nil
		}
	}
	return Magnet{}, fmt.Errorf("alldebrid: magnet %d not found", id)
}

// fileNode mirrors one entry of the /magnet/files tree. Folders carry
// nested entries under "e"; files carry an unlockable inner link under "l".
// Name/size/link use the short keys n/s/l.
type fileNode struct {
	Name    string     `json:"n"`
	Size    int64      `json:"s"`
	Link    string     `json:"l"`
	Entries []fileNode `json:"e"`
}

// flexInt64 decodes IDs sent either as numbers or strings (the files
// endpoint returns string IDs, status returns numeric ones).
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexInt64(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(v)
	return nil
}

func flattenFiles(prefix string, nodes []fileNode, out *[]MagnetFile) {
	for _, n := range nodes {
		p := n.Name
		if prefix != "" {
			p = prefix + "/" + n.Name
		}
		if n.Link != "" {
			*out = append(*out, MagnetFile{Link: n.Link, Filename: p, Size: n.Size})
		}
		if len(n.Entries) > 0 {
			flattenFiles(p, n.Entries, out)
		}
	}
}

// magnetFilesEntry is one entry of a /magnet/files response.
type magnetFilesEntry struct {
	ID    flexInt64  `json:"id"`
	Files []fileNode `json:"files"`
	Error *apiErr    `json:"error"`
}

func (c *Client) magnetFiles(ctx context.Context, base string, ids []int64) (map[int64][]MagnetFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{}
	for _, id := range ids {
		form.Add("id[]", strconv.FormatInt(id, 10))
	}
	var env envelope[struct {
		Magnets json.RawMessage `json:"magnets"`
	}]
	if err := c.postForm(ctx, base, "/magnet/files", form, &env); err != nil {
		return nil, err
	}
	if env.Status != "success" {
		return nil, env.Error
	}
	entries, err := decodeFlexList[magnetFilesEntry](env.Data.Magnets, func(e magnetFilesEntry) bool {
		return e.ID != 0 || len(e.Files) > 0 || e.Error != nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]MagnetFile, len(entries))
	for _, m := range entries {
		if m.Error != nil {
			continue // e.g. MAGNET_INVALID_ID for one entry; others still count
		}
		var files []MagnetFile
		flattenFiles("", m.Files, &files)
		out[int64(m.ID)] = files
	}
	return out, nil
}

// MagnetFiles returns the unlockable file trees for the given magnet IDs.
// It targets /v4/magnet/files and falls back to /v4.1 (docs are ambiguous
// about the namespace of this endpoint).
func (c *Client) MagnetFiles(ctx context.Context, ids ...int64) (map[int64][]MagnetFile, error) {
	if len(ids) == 0 {
		return map[int64][]MagnetFile{}, nil
	}
	out, err := c.magnetFiles(ctx, c.base, ids)
	if err != nil && isNotFound(err) {
		return c.magnetFiles(ctx, c.base41, ids)
	}
	return out, err
}

// UploadMagnet adds a magnet to the cloud and returns its ID.
// magnets= may be a magnet: URI or an http(s) URL to a .torrent.
// Per-item errors (e.g. MAGNET_INVALID_URI) are surfaced, not swallowed:
// the endpoint reports them inside an otherwise-successful response.
// uploadedMagnet is one entry of a /magnet/upload response.
type uploadedMagnet struct {
	ID     int64   `json:"id"`
	Magnet string  `json:"magnet"`
	Hash   string  `json:"hash"`
	Name   string  `json:"name"`
	Ready  bool    `json:"ready"`
	Error  *apiErr `json:"error"`
}

func (c *Client) UploadMagnet(ctx context.Context, magnet string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	form := url.Values{}
	form.Set("magnets[]", magnet)
	var env envelope[struct {
		Magnets json.RawMessage `json:"magnets"`
	}]
	if err := c.postForm(ctx, c.base, "/magnet/upload", form, &env); err != nil {
		return 0, err
	}
	if env.Status != "success" {
		return 0, env.Error
	}
	items, err := decodeFlexList[uploadedMagnet](env.Data.Magnets, func(u uploadedMagnet) bool {
		return u.ID != 0 || u.Error != nil || u.Magnet != ""
	})
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, errors.New("alldebrid: upload returned no magnet")
	}
	item := items[0]
	if item.Error != nil {
		return 0, item.Error
	}
	return item.ID, nil
}

// UploadFile pushes raw .torrent bytes to /v4/magnet/upload/file
// (multipart files[]) and returns the magnet ID. Per-item errors
// (e.g. MAGNET_INVALID_FILE) are surfaced, not swallowed.
func (c *Client) UploadFile(ctx context.Context, filename string, data []byte) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if c.apiKey == "" {
		return 0, errors.New("alldebrid: missing API key")
	}
	if len(data) == 0 {
		return 0, errors.New("alldebrid: empty torrent file")
	}
	if filename == "" {
		filename = "download.torrent"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("files[]", filename)
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(data); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}
	q := url.Values{}
	q.Set("apikey", c.apiKey) // docs example passes it in the query string here
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/magnet/upload/file?"+q.Encode(), &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, err
	}
	var env envelope[struct {
		Files json.RawMessage `json:"files"`
	}]
	if resp.StatusCode != http.StatusOK {
		var eenv envelope[json.RawMessage]
		if jerr := json.Unmarshal(body, &eenv); jerr == nil && eenv.Status == "error" && eenv.Error != nil {
			return 0, eenv.Error
		}
		return 0, fmt.Errorf("alldebrid: POST /magnet/upload/file: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("alldebrid: decode upload/file: %w", err)
	}
	if env.Status != "success" {
		return 0, env.Error
	}
	items, err := decodeFlexList[uploadedFile](env.Data.Files, func(f uploadedFile) bool {
		return f.ID != 0 || f.Error != nil || f.File != ""
	})
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, errors.New("alldebrid: upload/file returned no file")
	}
	item := items[0]
	if item.Error != nil {
		return 0, item.Error
	}
	return item.ID, nil
}

// uploadedFile is one entry of a /magnet/upload/file response.
type uploadedFile struct {
	File  string  `json:"file"`
	Name  string  `json:"name"`
	Size  int64   `json:"size"`
	Hash  string  `json:"hash"`
	Ready bool    `json:"ready"`
	ID    int64   `json:"id"`
	Error *apiErr `json:"error"`
}

// uploadOne resolves a single candidate into a cloud magnet ID.
// magnet: URIs go straight to /magnet/upload; http(s) .torrent links are
// fetched locally first (index download links are unreachable from
// AllDebrid's servers) and pushed via /magnet/upload/file.
// The magnet detection tolerates invisible junk (BOM, zero-width chars)
// that some indexers prepend to magnet URLs and that prefix matching
// would otherwise miss.
func (c *Client) uploadOne(ctx context.Context, cand string) (int64, error) {
	trimmed := strings.TrimSpace(cand)
	low := strings.ToLower(trimmed)
	if strings.HasPrefix(low, "magnet:") {
		return c.uploadMagnetURI(ctx, trimmed)
	}
	if i := strings.Index(low, "magnet:?xt="); i >= 0 {
		log.Printf("alldebrid: stripping %d-byte junk prefix from magnet candidate", i)
		return c.uploadMagnetURI(ctx, trimmed[i:])
	}
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		if c.TorrentFetch == nil {
			return 0, fmt.Errorf("torrent URL but no fetcher configured: %.80q", cand)
		}
		ft, err := c.TorrentFetch(ctx, cand)
		if err != nil {
			return 0, fmt.Errorf("fetch torrent: %w", err)
		}
		if ft.Magnet != "" {
			return c.uploadMagnetURI(ctx, ft.Magnet)
		}
		if len(ft.Data) == 0 {
			return 0, fmt.Errorf("fetch torrent: empty result for %.80q", cand)
		}
		id, err := c.UploadFile(ctx, ft.Filename, ft.Data)
		if err != nil {
			return 0, fmt.Errorf("torrent upload: %w", err)
		}
		return id, nil
	}
	return 0, fmt.Errorf("unrecognized candidate (quoted head=%.100q len=%d)", cand, len(cand))
}

func (c *Client) uploadMagnetURI(ctx context.Context, magnet string) (int64, error) {
	id, err := c.UploadMagnet(ctx, magnet)
	if err != nil {
		return 0, fmt.Errorf("magnet upload: %w", err)
	}
	return id, nil
}

// ErrStalled is returned by WaitReady when AllDebrid reports no activity
// for a magnet over consecutive polls. Index-reported seeders are often
// stale (and AllDebrid itself can show phantom seeders with zero movement),
// so the trigger is observed activity — bytes or speed — not just the
// seeder count. Callers should move on to the next candidate instead of
// burning the full wait budget on a dead torrent.
var ErrStalled = errors.New("alldebrid: torrent stalled (no activity)")

// Stall thresholds in consecutive dead polls (no new bytes, no speed):
// fast when AllDebrid also reports zero seeders, slower otherwise so big
// swarms get time to deliver a first byte. At the default 4s interval that
// is ~20s / ~48s inside the per-candidate wait budget.
const (
	stallPollsFast = 5
	stallPollsSlow = 12
)

// WaitReady polls MagnetStatus until Status == "Ready" or ctx/deadline ends.
// Cached torrents are typically Ready on the first poll. Torrents with no
// observed activity abort early with ErrStalled; active ones get the full
// budget.
func (c *Client) WaitReady(ctx context.Context, id int64, pollEvery time.Duration) (Magnet, error) {
	if pollEvery <= 0 {
		pollEvery = 4 * time.Second
	}
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	var prevDownloaded int64 = -1
	dead := 0
	for {
		m, err := c.MagnetStatus(ctx, id)
		if err != nil {
			return Magnet{}, err
		}
		if m.Status == StatusReady {
			return m, nil
		}
		if m.DownloadSpeed > 0 || (prevDownloaded >= 0 && m.Downloaded > prevDownloaded) {
			dead = 0 // alive: moving or pushing bytes
		} else {
			dead++
			limit := stallPollsSlow
			if m.Seeders == 0 {
				limit = stallPollsFast
			}
			if dead >= limit {
				return Magnet{}, fmt.Errorf("%w (magnet %d, seeders=%d, downloaded=%d)",
					ErrStalled, id, m.Seeders, m.Downloaded)
			}
		}
		prevDownloaded = m.Downloaded
		select {
		case <-ctx.Done():
			return Magnet{}, ctx.Err()
		case <-t.C:
		}
	}
}

// UnlockLink resolves an AllDebrid inner link to a direct CDN HTTPS URL.
func (c *Client) UnlockLink(ctx context.Context, link string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{}
	form.Set("link", link)
	var env envelope[struct {
		Link     string `json:"link"`
		Filename string `json:"filename"`
		Filesize int64  `json:"filesize"`
	}]
	if err := c.postForm(ctx, c.base, "/link/unlock", form, &env); err != nil {
		return "", err
	}
	if env.Status != "success" {
		return "", env.Error
	}
	if env.Data.Link == "" {
		return "", errors.New("alldebrid: unlock returned empty link")
	}
	return env.Data.Link, nil
}
