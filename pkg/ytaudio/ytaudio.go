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
	"strconv"
	"strings"
)

// Client is a yt-dlp-backed resolver. The zero value is not usable; use New.
type Client struct {
	bin       string
	cookies   string
	extraArgs []string
	enabled   bool
}

// Stream is a resolved audio source.
type Stream struct {
	URL         string
	ContentType string
}

// New builds a resolver for the given yt-dlp binary. bin may be empty to use
// the "yt-dlp" on PATH. cookiesFile, when non-empty, is passed to yt-dlp with
// --cookies (the escape hatch for YouTube bot checks). extraArgs are inserted
// before the search query (e.g. --extractor-args).
//
// Recent yt-dlp needs a JS runtime for YouTube's JS challenges. Deno is
// enabled by default, so nothing is added when it is present; otherwise, if
// Node.js (>= 22) is on PATH, --js-runtimes node is injected automatically.
// A caller-supplied --js-runtimes always wins.
func New(bin, cookiesFile string, extraArgs []string) *Client {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = "yt-dlp"
	}
	args := append([]string(nil), extraArgs...)
	if !hasFlag(args, "--js-runtimes") {
		if _, err := exec.LookPath("deno"); err != nil {
			if nodeUsable() {
				args = append(args, "--js-runtimes", "node")
			}
		}
	}
	c := &Client{bin: bin, cookies: strings.TrimSpace(cookiesFile), extraArgs: args}
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

// nodeUsable reports whether a Node.js new enough for yt-dlp's JS challenge
// solver (>= 22) is on PATH.
func nodeUsable() bool {
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		return false
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return err == nil && major >= 22
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
	cmd := exec.CommandContext(ctx, c.bin, resolveArgs(query, best, c.cookies, c.extraArgs)...)
	out, err := cmd.Output()
	if err != nil {
		return Stream{}, fmt.Errorf("ytaudio: %w%s", err, stderrSuffix(err))
	}
	raw := firstURL(string(out))
	if raw == "" {
		return Stream{}, errors.New("ytaudio: no url resolved")
	}
	return Stream{URL: raw, ContentType: contentTypeForURL(raw)}, nil
}

// stderrSuffix returns yt-dlp's stderr (captured by Cmd.Output into the
// ExitError) so the real reason — bot check, missing JS runtime, format
// unavailable — is visible in our logs instead of a bare "exit status 1".
func stderrSuffix(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	msg := strings.TrimSpace(string(ee.Stderr))
	if msg == "" {
		return ""
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	return ": " + msg
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
		"-g",
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	args = append(args, extra...)
	args = append(args, "ytsearch1:"+query)
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
