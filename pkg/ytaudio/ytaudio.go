// Package ytaudio resolves a direct, streamable audio URL for a track by
// shelling out to the yt-dlp binary. It is used only for URL resolution —
// the bytes are streamed by the caller — so yt-dlp never sits in the
// streaming path and no ffmpeg is required.
//
// The instant-playback preview uses a low-bitrate m4a (itag 140 when
// available): small, fast to start, and universally understood by Subsonic
// clients. The background canonical fetch asks for the best available audio.
package ytaudio

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// Client is a yt-dlp-backed resolver. The zero value is not usable; use New.
type Client struct {
	bin          string
	cookies      string
	extraArgs    []string
	fallbackArgs []string
	enabled      bool
}

// Stream is a resolved audio source.
type Stream struct {
	URL          string
	ContentType  string
	VideoID      string
	ThumbnailURL string
}

// New builds a resolver for the given yt-dlp binary. bin may be empty to use
// the "yt-dlp" on PATH. cookiesFile, when non-empty, is passed to yt-dlp with
// --cookies (the escape hatch for YouTube bot checks). extraArgs are inserted
// before the search query (e.g. --extractor-args).
//
// By default it selects YouTube's "android_sdkless" player client, which needs
// no JavaScript runtime and resolves roughly twice as fast as the web client's
// JS-challenge path. If that client ever fails (e.g. a video it can't access),
// Resolve retries once with the JS-capable defaults. A caller-supplied
// --extractor-args or --no-js-runtimes disables both the default and fallback.
func New(bin, cookiesFile string, extraArgs []string) *Client {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = "yt-dlp"
	}
	args := append([]string(nil), extraArgs...)
	var fallback []string
	if !hasFlag(args, "--extractor-args") && !hasFlag(args, "--no-js-runtimes") {
		fallback = append([]string{}, args...)
		args = append(args, "--extractor-args", "youtube:player_client=android_sdkless")
	}
	c := &Client{bin: bin, cookies: strings.TrimSpace(cookiesFile), extraArgs: args, fallbackArgs: fallback}
	if _, err := exec.LookPath(bin); err == nil {
		c.enabled = true
	}
	return c
}

// hasFlag reports whether args contains name, either bare or as name=value.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// Enabled reports whether the resolver can run (yt-dlp present, not disabled).
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// Version returns `yt-dlp --version`, or an error describing why it failed.
// Used for a startup diagnostic log.
func (c *Client) Version(ctx context.Context) (string, error) {
	if !c.Enabled() {
		return "", errors.New("yt-dlp not found")
	}
	out, err := exec.CommandContext(ctx, c.bin, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// warmURL is a stable, always-available music video used only to pay yt-dlp's
// one-time cold-start cost (loading the JS runtime and its EJS challenge
// solver) before a user request needs it.
const warmURL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

// Warm runs one throwaway resolve to populate the runtime/EJS caches so the
// first real request is fast. Safe to call in the background.
func (c *Client) Warm(ctx context.Context) error {
	if !c.Enabled() {
		return errors.New("ytaudio: disabled")
	}
	args := []string{"--no-playlist", "--no-warnings", "--no-progress", "-f", "140/bestaudio/best", "-g"}
	if c.cookies != "" {
		args = append(args, "--cookies", c.cookies)
	}
	args = append(args, c.extraArgs...)
	args = append(args, warmURL)
	if _, err := exec.CommandContext(ctx, c.bin, args...).Output(); err != nil {
		return fmt.Errorf("%w%s", err, failureDetail(ctx, err))
	}
	return nil
}

// Resolve searches YouTube for "artist title" and returns a direct audio URL.
// best selects the highest-quality audio (the canonical fallback); otherwise a
// low-bitrate m4a is chosen for fast, instant playback.
func (c *Client) Resolve(ctx context.Context, artist, title string, best bool) (Stream, error) {
	if !c.Enabled() {
		return Stream{}, errors.New("ytaudio: disabled")
	}
	query := strings.TrimSpace(strings.Join(strings.Fields(artist+" "+title), " "))
	if query == "" {
		return Stream{}, errors.New("ytaudio: empty query")
	}
	st, err := c.try(ctx, query, best, c.extraArgs)
	if err == nil || c.fallbackArgs == nil {
		return st, err
	}
	// The fast JS-less client failed; retry once with the JS-capable default
	// (the JS runtime, if installed, handles YouTube's challenge).
	if st2, err2 := c.try(ctx, query, best, c.fallbackArgs); err2 == nil {
		return st2, nil
	}
	return Stream{}, err
}

// try runs one resolve with the given extra arguments.
func (c *Client) try(ctx context.Context, query string, best bool, extra []string) (Stream, error) {
	cmd := exec.CommandContext(ctx, c.bin, resolveArgs(query, best, c.cookies, extra)...)
	out, err := cmd.Output()
	if err != nil {
		return Stream{}, fmt.Errorf("ytaudio: %w%s", err, failureDetail(ctx, err))
	}
	st := parseStream(string(out))
	if st.URL == "" {
		return Stream{}, errors.New("ytaudio: no url resolved")
	}
	return st, nil
}

func parseStream(out string) Stream {
	var st Stream
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "__MSID__"):
			st.VideoID = cleanMetadata(strings.TrimPrefix(line, "__MSID__"))
		case strings.HasPrefix(line, "__MSTHUMB__"):
			st.ThumbnailURL = cleanMetadata(strings.TrimPrefix(line, "__MSTHUMB__"))
		}
	}
	st.URL = firstURL(out)
	if st.URL != "" {
		st.ContentType = contentTypeForURL(st.URL)
	}
	if st.ThumbnailURL == "" && st.VideoID != "" {
		st.ThumbnailURL = ThumbnailURLForVideo(st.VideoID)
	}
	return st
}

func cleanMetadata(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "NA" || v == "None" {
		return ""
	}
	return v
}

// ThumbnailURLForVideo returns a stable YouTube thumbnail for a video id.
func ThumbnailURLForVideo(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
}

// failureDetail explains a yt-dlp failure. It prefers yt-dlp's own stderr
// (bot check, missing JS runtime, format unavailable) but, when stderr is
// empty, distinguishes the three silent-kill causes: our context deadline, a
// client-cancelled request, or an external SIGKILL (usually the OOM killer).
func failureDetail(ctx context.Context, err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return ": " + truncate(strings.Join(strings.Fields(msg), " "), 600)
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return " (timed out - YouTube extraction too slow)"
	case errors.Is(ctx.Err(), context.Canceled):
		return " (cancelled by client)"
	}
	if errors.As(err, &ee) && ee.ProcessState != nil {
		return " (killed externally: " + ee.ProcessState.String() + "; likely OOM - raise the container memory limit)"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// resolveArgs builds the yt-dlp argument list. Kept separate so it can be
// unit-tested without a real yt-dlp binary.
func resolveArgs(query string, best bool, cookies string, extra []string) []string {
	format := "140/bestaudio[ext=m4a]/bestaudio/best"
	if best {
		format = "bestaudio[ext=m4a]/bestaudio/best"
	}
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--no-progress",
		"-f", format,
		"--print", "__MSID__%(id)s",
		"--print", "__MSTHUMB__%(thumbnail)s",
		"-g",
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	args = append(args, extra...)
	args = append(args, "ytsearch1:"+query)
	return args
}

// firstURL returns the first direct, streamable URL from yt-dlp's -g output.
// HLS manifests (.m3u8) are skipped: they are playlists, not media, and would
// be truncated/cached as garbage by the caller. If only a manifest is offered,
// no URL is returned (the caller falls back to its other source).
func firstURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		u := strings.TrimSpace(line)
		if !strings.HasPrefix(u, "http") {
			continue
		}
		if strings.Contains(u, ".m3u8") || strings.Contains(u, "manifest.googlevideo.com") {
			continue
		}
		return u
	}
	return ""
}

// contentTypeForURL derives the media type from a googlevideo URL's mime
// parameter, falling back to audio/mp4 (the itag-140 container).
func contentTypeForURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if m := u.Query().Get("mime"); m != "" {
			return m
		}
	}
	return "audio/mp4"
}
