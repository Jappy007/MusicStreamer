// Command debridsubsonic is an ultra-lightweight Subsonic-compatible music
// server that resolves tracks via local LRU disk cache -> AllDebrid cloud ->
// Prowlarr scrape + AllDebrid upload.
//
// Tiny static binary, <30 MB RAM: stdlib-only, streaming I/O throughout,
// O(files) cache index, no DB, no in-memory audio buffering.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/debridsubsonic/debridsubsonic/pkg/cache"
	"github.com/debridsubsonic/debridsubsonic/pkg/debrid"
	"github.com/debridsubsonic/debridsubsonic/pkg/library"
	"github.com/debridsubsonic/debridsubsonic/pkg/metadata"
	"github.com/debridsubsonic/debridsubsonic/pkg/soulseek"
	"github.com/debridsubsonic/debridsubsonic/pkg/subsonic"
)

// Config is populated from environment variables.
type Config struct {
	AllDebridAPIKey  string
	SubsonicUser     string
	SubsonicPass     string
	CacheDir         string
	CacheMaxBytes    int64
	ProwlarrURL      string
	ProwlarrAPIKey   string
	MinSeeders       int
	SlskdURL         string
	SlskdAPIKey      string
	SlskdDownloadDir string
	Port             string
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	cacheGB, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("CACHE_SIZE_GB")), 64)
	if cacheGB <= 0 {
		cacheGB = 10
	}
	minSeeders, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("MIN_SEEDERS")))
	if minSeeders < 0 {
		minSeeders = 0
	}
	return Config{
		AllDebridAPIKey:  strings.TrimSpace(os.Getenv("ALLDEBRID_API_KEY")),
		SubsonicUser:     getenv("SUBSONIC_USER", "admin"),
		SubsonicPass:     getenv("SUBSONIC_PASS", "admin"),
		CacheDir:         getenv("CACHE_DIR", "/data/cache"),
		CacheMaxBytes:    int64(cacheGB * 1024 * 1024 * 1024),
		ProwlarrURL:      strings.TrimSpace(os.Getenv("PROWLARR_URL")),
		ProwlarrAPIKey:   strings.TrimSpace(os.Getenv("PROWLARR_API_KEY")),
		MinSeeders:       minSeeders,
		SlskdURL:         strings.TrimSpace(os.Getenv("SLSKD_URL")),
		SlskdAPIKey:      strings.TrimSpace(os.Getenv("SLSKD_API_KEY")),
		SlskdDownloadDir: strings.TrimSpace(os.Getenv("SLSKD_DOWNLOAD_DIR")),
		Port:             getenv("PORT", "4533"),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cfg := loadConfig()

	if cfg.AllDebridAPIKey == "" {
		log.Println("WARN: ALLDEBRID_API_KEY is empty; Tier 2/3 streaming will fail until set")
	}

	// LRU disk cache (Tier 1).
	store, err := cache.New(cfg.CacheDir, cfg.CacheMaxBytes)
	if err != nil {
		log.Fatalf("cache init: %v", err)
	}
	n, total, max := store.Stats()
	log.Printf("cache: dir=%s entries=%d total=%.2fGiB max=%.2fGiB",
		cfg.CacheDir, n, float64(total)/(1<<30), float64(max)/(1<<30))

	// Background LRU cleaner: enforce ceiling every 60s (Store also
	// enforces synchronously after each write).
	stopCleaner := make(chan struct{})
	defer close(stopCleaner)
	go cache.StartCleaner(store, time.Minute, stopCleaner)

	// Collaborators.
	meta := metadata.New(nil)
	ad := debrid.New(cfg.AllDebridAPIKey, nil)
	indexer := debrid.NewProwlarr(cfg.ProwlarrURL, cfg.ProwlarrAPIKey, nil)
	indexer.MinSeeders = cfg.MinSeeders
	if indexer.Enabled() {
		log.Printf("prowlarr: enabled (%s, min_seeders=%d)", cfg.ProwlarrURL, cfg.MinSeeders)
	} else {
		log.Println("prowlarr: disabled; Tier 3 falls back to built-in public indexes")
	}
	public := debrid.NewPublicIndex(nil)
	sk := soulseek.New(cfg.SlskdURL, cfg.SlskdAPIKey, cfg.SlskdDownloadDir, nil)
	if sk.Enabled() {
		if fi, err := os.Stat(sk.DownloadDir()); err != nil || !fi.IsDir() {
			log.Printf("soulseek: WARNING download dir %s not visible — mount slskd's downloads volume there (SLSKD_DOWNLOAD_DIR overrides the %s default)",
				sk.DownloadDir(), soulseek.DefaultDownloadDir)
		} else {
			log.Printf("soulseek: enabled (%s -> %s)", cfg.SlskdURL, sk.DownloadDir())
		}
	} else {
		log.Println("soulseek: disabled; set SLSKD_URL to enable")
	}
	// Index download links are served by Prowlarr itself (LAN-only,
	// key-gated): fetch them here so AllDebrid receives real .torrent bytes.
	ad.TorrentFetch = func(ctx context.Context, rawURL string) (debrid.FetchedTorrent, error) {
		return indexer.FetchDownload(ctx, rawURL)
	}

	// Local music library (Tier 0): plays Lidarr's completed downloads
	// straight from disk. Empty/unset MUSIC_DIR disables the tier.
	var lib *library.Library
	if dir := strings.TrimSpace(os.Getenv("MUSIC_DIR")); dir != "" {
		lib, err = library.New(dir)
		if err != nil {
			log.Fatalf("library init: %v", err)
		}
		log.Printf("library: dir=%s files=%d", dir, lib.Stats())
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-stopCleaner:
					return
				case <-t.C:
					if err := lib.Scan(); err != nil {
						log.Printf("library rescan: %v", err)
					}
				}
			}
		}()
	}

	srv := subsonic.NewServer(cfg.SubsonicUser, cfg.SubsonicPass, meta, ad, indexer, public, sk, lib, store, nil)

	mux := http.NewServeMux()
	subsonic.Route(mux, srv)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           subsonic.WithAccessLog(subsonic.WithCORS(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: audio streams are long-lived; the request
		// context (client disconnect) ends them instead.
		IdleTimeout: 120 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutdown: draining...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	log.Printf("DebridSubsonic listening on :%s (user=%s)", cfg.Port, cfg.SubsonicUser)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
