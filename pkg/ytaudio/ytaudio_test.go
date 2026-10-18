package ytaudio

import (
	"context"
	"strings"
	"testing"
)

func TestResolveArgsPreview(t *testing.T) {
	args := resolveArgs("Boards of Canada Roygbiv", false, "")
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
	args := resolveArgs("x y", true, "/data/cookies.txt")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "bestaudio[ext=m4a]/bestaudio/best") {
		t.Fatalf("best format not selected: %v", args)
	}
	if !strings.Contains(joined, "--cookies /data/cookies.txt") {
		t.Fatalf("cookies not passed: %v", args)
	}
	if args[0] != "--cookies" {
		t.Fatalf("cookies flags should lead: %v", args)
	}
}

func TestFirstURL(t *testing.T) {
	if got := firstURL("\n\n  https://x/y  \n"); got != "https://x/y" {
		t.Fatalf("firstURL = %q", got)
	}
	if got := firstURL("   "); got != "" {
		t.Fatalf("firstURL empty = %q", got)
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
	c := New("definitely-not-a-real-binary-xyz", "")
	if c.Enabled() {
		t.Fatal("expected disabled for missing binary")
	}
	if _, err := c.Resolve(context.Background(), "a", "b", false); err == nil {
		t.Fatal("expected error when disabled")
	}
}
