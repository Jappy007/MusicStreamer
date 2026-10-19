package ytaudio

import (
	"strings"
	"testing"
)

func TestParseStreamMetadata(t *testing.T) {
	out := "__MSID__dQw4w9WgXcQ\n" +
		"__MSTHUMB__https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg\n" +
		"https://manifest.googlevideo.com/api/manifest/hls_playlist/index.m3u8\n" +
		"https://rr2.googlevideo.com/videoplayback?itag=140&mime=audio%2Fmp4\n"
	st := parseStream(out)
	if st.VideoID != "dQw4w9WgXcQ" {
		t.Fatalf("video id = %q", st.VideoID)
	}
	if st.ThumbnailURL != "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg" {
		t.Fatalf("thumb = %q", st.ThumbnailURL)
	}
	if !strings.Contains(st.URL, "itag=140") || st.ContentType != "audio/mp4" {
		t.Fatalf("stream = %+v", st)
	}
}

func TestParseStreamDerivesThumbnail(t *testing.T) {
	st := parseStream("__MSID__abc123\nhttps://x/videoplayback?mime=audio%2Fmp4")
	if st.ThumbnailURL != "https://i.ytimg.com/vi/abc123/hqdefault.jpg" {
		t.Fatalf("derived thumb = %q", st.ThumbnailURL)
	}
}

func TestResolveArgsRequestsMetadata(t *testing.T) {
	args := resolveArgs("x", false, "", nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "__MSID__%(id)s") || !strings.Contains(joined, "__MSTHUMB__%(thumbnail)s") {
		t.Fatalf("metadata prints missing: %v", args)
	}
}
