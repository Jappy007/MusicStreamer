package audioinfo

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "track")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFLACDuration(t *testing.T) {
	b := make([]byte, 42)
	copy(b, "fLaC")
	b[4] = 0x80
	b[5], b[6], b[7] = 0, 0, 34
	v := uint64(44100)<<44 | uint64(88200)
	binary.BigEndian.PutUint64(b[18:26], v)
	info, err := ProbeFile(writeTemp(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "flac" || info.Duration != 2 {
		t.Fatalf("got %+v", info)
	}
}

func TestWAVDuration(t *testing.T) {
	data := make([]byte, 16000)
	b := make([]byte, 0, 44+len(data))
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 8000)
	b = binary.LittleEndian.AppendUint32(b, 8000)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 8)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	info, err := ProbeFile(writeTemp(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "wav" || info.Duration != 2 {
		t.Fatalf("got %+v", info)
	}
}

func TestAIFFDuration(t *testing.T) {
	b := make([]byte, 12+8+18)
	copy(b, "FORM")
	binary.BigEndian.PutUint32(b[4:8], uint32(len(b)-8))
	copy(b[8:], "AIFFCOMM")
	binary.BigEndian.PutUint32(b[16:20], 18)
	binary.BigEndian.PutUint16(b[20:22], 1)
	binary.BigEndian.PutUint32(b[22:26], 88200)
	binary.BigEndian.PutUint16(b[26:28], 16)
	binary.BigEndian.PutUint16(b[28:30], 16398)
	binary.BigEndian.PutUint64(b[30:38], uint64(44100)<<48)
	info, err := ProbeFile(writeTemp(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "aiff" || info.SampleRate != 44100 || info.Duration != 2 {
		t.Fatalf("got %+v", info)
	}
}

func oggPage(granule uint64, payload []byte) []byte {
	nseg := (len(payload) + 254) / 255
	if nseg == 0 {
		nseg = 1
	}
	page := make([]byte, 27+nseg+len(payload))
	copy(page, "OggS")
	page[4] = 0
	binary.LittleEndian.PutUint64(page[6:14], granule)
	page[26] = byte(nseg)
	left := len(payload)
	for i := 0; i < nseg; i++ {
		n := left
		if n > 255 {
			n = 255
		}
		page[27+i] = byte(n)
		left -= n
	}
	copy(page[27+nseg:], payload)
	return page
}

func TestOggOpusDuration(t *testing.T) {
	head := make([]byte, 16)
	copy(head, "OpusHead")
	head[9] = 2
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	b := append(oggPage(0, head), oggPage(96000, []byte("tail"))...)
	info, err := ProbeFile(writeTemp(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "opus" || info.SampleRate != 48000 || info.Duration != 2 {
		t.Fatalf("got %+v", info)
	}
}

func TestMP3CBRDuration(t *testing.T) {
	frame := make([]byte, 417)
	frame[0], frame[1], frame[2], frame[3] = 0xFF, 0xFB, 0x90, 0x00
	b := make([]byte, 0, len(frame)*39)
	for i := 0; i < 39; i++ {
		b = append(b, frame...)
	}
	info, err := ProbeFile(writeTemp(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "mp3" || info.SampleRate != 44100 || info.BitRate != 128 || info.Duration != 1 {
		t.Fatalf("got %+v", info)
	}
}
