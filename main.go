// Command MusicStreamer is an ultra-lightweight Subsonic-compatible music
// server that resolves tracks through the local LRU disk cache and Soulseek
// (slskd) only — no external indexers, no persistent database.
//
// Tiny static binary, <30 MB RAM: stdlib-only, streaming I/O throughout,
// O(files) cache index, no DB, no in-memory audio buffering.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/musicstreamer/musicstreamer/pkg/cache"
	"github.com/musicstreamer/musicstreamer/pkg/metadata"
	"github.com/musicstreamer/musicstreamer/pkg/soulseek"
	"github.com/musicstreamer/musicstreamer/pkg/subsonic"
	"github.com/musicstreamer/musicstreamer/pkg/ytaudio"
)

// Config is populated from environment variables.
type Config struct {
	SubsonicUser     string
	SubsonicPass     string
	CacheDir         string
	CacheMaxBytes    int64
	PreviewSizeGB    float64
	YoutubeEnabled   bool
	YtDlpBin         string
	YtDlpCookies     string
	YtDlpArgs        []string
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

// logYoutubeRuntime reports which JS runtime yt-dlp will use. YouTube
// extraction needs one (deno is enabled by default; node must be selected via
// YTDLP_ARGS="--js-runtimes node"). A missing runtime is the most common cause
// of silent extraction failures.
func logYoutubeRuntime() {
	for _, rt := range []string{"deno", "node", "bun", "quickjs"} {
		if p, err := exec.LookPath(rt); err == nil {
			log.Printf("youtube: JS runtime %q found at %s", rt, p)
			return
		}
	}
	log.Printf("youtube: WARNING no JS runtime (deno/node/bun/quickjs) on PATH - YouTube extraction will likely fail; install deno in the image or set YTDLP_ARGS")
}

func loadConfig() Config {
	cacheGB, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("CACHE_SIZE_GB")), 64)
	if cacheGB <= 0 {
		cacheGB = 10
	}
	previewGB, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("PREVIEW_SIZE_GB")), 64)
	if previewGB <= 0 {
		previewGB = 2
	}
	ytEnabled := true
	if v := strings.TrimSpace(os.Getenv("YOUTUBE_ENABLED")); v != "" {
		ytEnabled = !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no"))
	}
	return Config{
		SubsonicUser:     getenv("SUBSONIC_USER", "admin"),
		SubsonicPass:     getenv("SUBSONIC_PASS", "admin"),
		CacheDir:         getenv("CACHE_DIR", "/data/cache"),
		CacheMaxBytes:    int64(cacheGB * 1024 * 1024 * 1024),
		PreviewSizeGB:    previewGB,
		YoutubeEnabled:   ytEnabled,
		YtDlpBin:         getenv("YTDLP_PATH", "yt-dlp"),
		YtDlpCookies:     strings.TrimSpace(os.Getenv("YTDLP_COOKIES")),
		YtDlpArgs:        strings.Fields(os.Getenv("YTDLP_ARGS")),
		SlskdURL:         strings.TrimSpace(os.Getenv("SLSKD_URL")),
		SlskdAPIKey:      strings.TrimSpace(os.Getenv("SLSKD_API_KEY")),
		SlskdDownloadDir: strings.TrimSpace(os.Getenv("SLSKD_DOWNLOAD_DIR")),
		Port:             getenv("PORT", "4533"),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cfg := loadConfig()

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

	// Preview cache (lossy instant-playback files), separate from the full
	// cache so previews never appear in the browsable library.
	preview, err := cache.New(filepath.Join(cfg.CacheDir, "preview"), int64(cfg.PreviewSizeGB*1024*1024*1024))
	if err != nil {
		log.Fatalf("preview cache init: %v", err)
	}
	go cache.StartCleaner(preview, time.Minute, stopCleaner)

	// Metadata is used only for library browsing/search (Deezer/MusicBrainz).
	meta := metadata.New(nil)

	// Instant-playback previews via yt-dlp (on by default; needs yt-dlp on PATH).
	var yt *ytaudio.Client
	if cfg.YoutubeEnabled {
		yt = ytaudio.New(cfg.YtDlpBin, cfg.YtDlpCookies, cfg.YtDlpArgs)
		if yt.Enabled() {
			vctx, vcancel := context.WithTimeout(context.Background(), 10*time.Second)
			ver, verr := yt.Version(vctx)
			vcancel()
			if verr != nil {
				log.Printf("youtube: enabled via %s but version check failed: %v", cfg.YtDlpBin, verr)
			} else {
				log.Printf("youtube: enabled via %s (version %s, preview budget %.1fGiB, cookies=%v, extra args=%v)",
					cfg.YtDlpBin, ver, cfg.PreviewSizeGB, cfg.YtDlpCookies != "", cfg.YtDlpArgs)
			}
			logYoutubeRuntime()
		} else {
			log.Printf("youtube: %s not found on PATH - instant playback disabled", cfg.YtDlpBin)
		}
	} else {
		log.Println("youtube: disabled (YOUTUBE_ENABLED=false)")
	}

	// Tier 3: Soulseek direct via slskd — the fetch source. Disabled when
	// SLSKD_URL is unset (cache-only server).
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

	srv := subsonic.NewServer(cfg.SubsonicUser, cfg.SubsonicPass, meta, sk, yt, store, preview, nil)

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

	log.Printf("MusicStreamer listening on :%s (user=%s)", cfg.Port, cfg.SubsonicUser)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
