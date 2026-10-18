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
	bin     string
	cookies string
	enabled bool
}

// Stream is a resolved audio source.
type Stream struct {
	URL         string
	ContentType string
}

// New builds a resolver for the given yt-dlp binary. bin may be empty to use
// the "yt-dlp" on PATH. cookiesFile, when non-empty, is passed to yt-dlp with
// --cookies (the escape hatch for YouTube bot checks). The client is only
// enabled when the binary is found on PATH.
func New(bin, cookiesFile string) *Client {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = "yt-dlp"
	}
	c := &Client{bin: bin, cookies: strings.TrimSpace(cookiesFile)}
	if _, err := exec.LookPath(bin); err == nil {
		c.enabled = true
	}
	return c
}

// Enabled reports whether the resolver can run (yt-dlp present, not disabled).
func (c *Client) Enabled() bool { return c != nil && c.enabled }

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
	cmd := exec.CommandContext(ctx, c.bin, resolveArgs(query, best, c.cookies)...)
	out, err := cmd.Output()
	if err != nil {
		return Stream{}, fmt.Errorf("ytaudio: %w", err)
	}
	raw := firstURL(string(out))
	if raw == "" {
		return Stream{}, errors.New("ytaudio: no url resolved")
	}
	return Stream{URL: raw, ContentType: contentTypeForURL(raw)}, nil
}

// resolveArgs builds the yt-dlp argument list. Kept separate so it can be
// unit-tested without a real yt-dlp binary.
func resolveArgs(query string, best bool, cookies string) []string {
	format := "140/bestaudio[ext=m4a]/bestaudio/best"
	if best {
		format = "bestaudio[ext=m4a]/bestaudio/best"
	}
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--no-progress",
		"-f", format,
		"-g",
		"ytsearch1:" + query,
	}
	if cookies != "" {
		args = append([]string{"--cookies", cookies}, args...)
	}
	return args
}

// firstURL returns the first non-empty line of yt-dlp's -g output.
func firstURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
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
