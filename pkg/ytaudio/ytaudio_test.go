package ytaudio

import (
	"context"
	"strings"
	"testing"
)

func TestResolveArgsPreview(t *testing.T) {
	args := resolveArgs("Boards of Canada Roygbiv", false, "", nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ytsearch1:Boards of Canada Roygbiv") {
		t.Fatalf("missing search term: %v", args)
	}
	if !strings.Contains(joined, "140/bestaudio[ext=m4a]/bestaudio/best") {
		t.Fatalf("preview should prefer itag 140: %v", args)
	}
	if strings.Contains(joined, "--cookies") {
		t.Fatalf("no cookies expected: %v", args)
	}
	if args[len(args)-1] != "ytsearch1:Boards of Canada Roygbiv" {
		t.Fatalf("query must be last: %v", args)
	}
}

func TestResolveArgsBest(t *testing.T) {
	args := resolveArgs("x y", true, "/data/cookies.txt", []string{"--js-runtimes", "node"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "bestaudio[ext=m4a]/bestaudio/best") {
		t.Fatalf("best format not selected: %v", args)
	}
	if !strings.Contains(joined, "--cookies /data/cookies.txt") {
		t.Fatalf("cookies not passed: %v", args)
	}
	if !strings.Contains(joined, "--js-runtimes node") {
		t.Fatalf("extra args not passed: %v", args)
	}
	if args[len(args)-1] != "ytsearch1:x y" {
		t.Fatalf("query must be last: %v", args)
	}
}

func TestFirstURL(t *testing.T) {
	if got := firstURL("\n\n  https://x/y  \n"); got != "https://x/y" {
		t.Fatalf("firstURL = %q", got)
	}
	if got := firstURL("   "); got != "" {
		t.Fatalf("firstURL empty = %q", got)
	}
	// HLS manifest first, then a direct URL: the manifest must be skipped.
	out := "https://manifest.googlevideo.com/api/manifest/hls_playlist/index.m3u8\nhttps://rr2.googlevideo.com/videoplayback?itag=251"
	if got := firstURL(out); got != "https://rr2.googlevideo.com/videoplayback?itag=251" {
		t.Fatalf("manifest not skipped: %q", got)
	}
	// Manifest-only output yields nothing rather than a playlist URL.
	if got := firstURL("https://x/y/index.m3u8"); got != "" {
		t.Fatalf("manifest-only = %q", got)
	}
}

func TestContentTypeForURL(t *testing.T) {
	u := "https://rr3.googlevideo.com/videoplayback?mime=audio%2Fmp4&itag=140"
	if got := contentTypeForURL(u); got != "audio/mp4" {
		t.Fatalf("mime parse = %q", got)
	}
	if got := contentTypeForURL("not a url"); got != "audio/mp4" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestNewDisabledWhenMissing(t *testing.T) {
	c := New("definitely-not-a-real-binary-xyz", "", nil)
	if c.Enabled() {
		t.Fatal("expected disabled for missing binary")
	}
	if _, err := c.Resolve(context.Background(), "a", "b", false); err == nil {
		t.Fatal("expected error when disabled")
	}
}
