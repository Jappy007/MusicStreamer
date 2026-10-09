package debrid

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// PublicIndex searches free public torrent indexes that need no API key or
// local indexer. Currently backed by torrents-csv (DHT crawl). It is Tier
// 3's zero-config fallback when Prowlarr is absent or returns nothing.
type PublicIndex struct {
	http *http.Client
	csv  string // overridable for tests
}

// NewPublicIndex builds the zero-config public search. Pass nil for defaults.
func NewPublicIndex(httpClient *http.Client) *PublicIndex {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &PublicIndex{
		http: httpClient,
		csv:  "https://api.torrents-csv.com",
	}
}

type scoredLink struct {
	magnet  string
	seeders int
	name    string
}

// Search returns up to limit magnet links for query, best-seeded first.
func (p *PublicIndex) Search(ctx context.Context, query string, limit int) ([]string, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	links := p.searchCSV(ctx, query)
	log.Printf("public index: torrents-csv query=%q links=%d", query, len(links))
	return topMagnets(links, limit), nil
}

// searchCSV queries the torrents-csv DHT search API.
func (p *PublicIndex) searchCSV(ctx context.Context, query string) []scoredLink {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.csv+"/service/search?q="+url.QueryEscape(query), nil)
	if err != nil {
		log.Printf("public index: torrents-csv: %v", err)
		return nil
	}
	req.Header.Set("User-Agent", "DebridSubsonic/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		log.Printf("public index: torrents-csv: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("public index: torrents-csv: HTTP %d", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		log.Printf("public index: torrents-csv: %v", err)
		return nil
	}
	var env struct {
		Torrents []struct {
			Infohash  string `json:"infohash"`
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
			Seeders   int    `json:"seeders"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		log.Printf("public index: torrents-csv decode: %v", err)
		return nil
	}
	var out []scoredLink
	for _, r := range env.Torrents {
		m, s := toMagnet(r.Name, r.Infohash, r.Seeders)
		if m == "" {
			continue
		}
		out = append(out, scoredLink{m, s, r.Name})
	}
	return out
}

// toMagnet builds a magnet link, or "" when the row is unusable.
// Video files are skipped (a concert DVD wastes an AllDebrid slot when the
// user asked for audio); lossless names get a ranking bonus.
func toMagnet(name, hash string, seeders int) (string, int) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != 40 || !isHex(hash) {
		return "", 0
	}
	low := strings.ToLower(name)
	for _, ext := range []string{".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm"} {
		if strings.HasSuffix(low, ext) {
			return "", 0
		}
	}
	if strings.Contains(low, "x264") || strings.Contains(low, "x265") ||
		strings.Contains(low, "bluray") || strings.Contains(low, "bdrip") {
		return "", 0
	}
	score := seeders
	switch {
	case strings.Contains(low, "flac"):
		score += 20
	case strings.Contains(low, "mp3") || strings.Contains(low, "320"):
		score += 5
	}
	return "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(name), score
}

// topMagnets dedupes by info-hash and returns the best magnets first.
func topMagnets(in []scoredLink, limit int) []string {
	seen := make(map[string]bool)
	var uniq []scoredLink
	for _, l := range in {
		key := btih(l.magnet)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, l)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].seeders > uniq[j].seeders })
	if len(uniq) > limit {
		uniq = uniq[:limit]
	}
	out := make([]string, 0, len(uniq))
	for _, l := range uniq {
		out = append(out, l.magnet)
	}
	return out
}

func btih(magnet string) string {
	i := strings.Index(magnet, "btih:")
	if i == -1 {
		return ""
	}
	rest := magnet[i+5:]
	if j := strings.Index(rest, "&"); j != -1 {
		rest = rest[:j]
	}
	return strings.ToLower(rest)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
