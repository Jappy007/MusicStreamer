// Package audioinfo identifies an audio file's container/codec and its
// sample rate, bit depth and channel count by reading only the header bytes
// (or a few seeks for MP4). It is stdlib-only: no ffprobe, no tag library.
//
// Cached files are stored by opaque song ID with no extension, so playback
// quality can only be recovered by sniffing the bytes themselves.
package audioinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"strings"
)

// Info describes the audio stream found in a file. Zero values mean "not
// known / not applicable" (e.g. bit depth is 0 for lossy codecs).
type Info struct {
	Format     string // flac, mp3, wav, aiff, ogg, opus, m4a, alac, aac, wma
	SampleRate int    // Hz
	BitDepth   int    // bits per sample (lossless only)
	Channels   int
	BitRate    int // kbps (MP3 only; 0 when unknown)
}

// ErrUnknown is returned when the file is not a recognized audio format.
var ErrUnknown = errors.New("audioinfo: unknown format")

const prefixLen = 64 << 10

// ProbeFile opens path and reports its audio properties.
func ProbeFile(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	buf := make([]byte, prefixLen)
	n, _ := io.ReadFull(f, buf) // short files return ErrUnexpectedEOF
	buf = buf[:n]
	return probe(f, buf)
}

func probe(f *os.File, b []byte) (Info, error) {
	switch {
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return probeFLAC(b)
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return probeWAV(b)
	case len(b) >= 12 && string(b[:4]) == "FORM" &&
		(string(b[8:12]) == "AIFF" || string(b[8:12]) == "AIFC"):
		return probeAIFF(b)
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return probeOgg(b), nil
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return probeMP4(f)
	case len(b) >= 3 && string(b[:3]) == "ID3":
		if off := id3v2Size(b); off >= 0 && off < len(b) {
			if info, ok := probeMP3(b[off:]); ok {
				return info, nil
			}
		}
		return Info{Format: "mp3"}, nil
	case len(b) >= 2 && b[0] == 0xFF:
		if info, ok := probeMP3(b); ok {
			return info, nil
		}
		if b[1]&0xF0 == 0xF0 {
			return Info{Format: "aac"}, nil // ADTS
		}
	case len(b) >= 16 && isASF(b):
		return Info{Format: "wma"}, nil
	}
	return Info{}, ErrUnknown
}

// --- FLAC -----------------------------------------------------------------

func probeFLAC(b []byte) (Info, error) {
	off := 4
	for off+4 <= len(b) {
		last := b[off]&0x80 != 0
		typ := b[off] & 0x7F
		length := int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
		if typ == 0 { // STREAMINFO
			if off+4+18 > len(b) {
				break
			}
			v := binary.BigEndian.Uint64(b[off+4+10 : off+4+18])
			return Info{
				Format:     "flac",
				SampleRate: int(v >> 44),
				BitDepth:   int((v>>36)&0x1F) + 1,
				Channels:   int((v>>41)&0x7) + 1,
			}, nil
		}
		off += 4 + length
		if last {
			break
		}
	}
	return Info{Format: "flac"}, nil
}

// --- WAV ------------------------------------------------------------------

func probeWAV(b []byte) (Info, error) {
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		sz := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		if id == "fmt " && off+8+16 <= len(b) {
			d := b[off+8:]
			return Info{
				Format:     "wav",
				Channels:   int(binary.LittleEndian.Uint16(d[2:4])),
				SampleRate: int(binary.LittleEndian.Uint32(d[4:8])),
				BitDepth:   int(binary.LittleEndian.Uint16(d[14:16])),
			}, nil
		}
		if sz < 0 {
			break
		}
		off += 8 + sz + (sz & 1)
	}
	return Info{Format: "wav"}, nil
}

// --- AIFF -----------------------------------------------------------------

func probeAIFF(b []byte) (Info, error) {
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		sz := int(binary.BigEndian.Uint32(b[off+4 : off+8]))
		if id == "COMM" && off+8+18 <= len(b) {
			d := b[off+8:]
			return Info{
				Format:     "aiff",
				Channels:   int(binary.BigEndian.Uint16(d[0:2])),
				BitDepth:   int(binary.BigEndian.Uint16(d[6:8])),
				SampleRate: extFloat(d[8:18]),
			}, nil
		}
		if sz < 0 {
			break
		}
		off += 8 + sz + (sz & 1)
	}
	return Info{Format: "aiff"}, nil
}

// extFloat decodes an 80-bit IEEE 754 extended-precision float (AIFF rate).
func extFloat(b []byte) int {
	if len(b) < 10 {
		return 0
	}
	expo := int(binary.BigEndian.Uint16(b[0:2]) & 0x7FFF)
	mant := binary.BigEndian.Uint64(b[2:10])
	if expo == 0 && mant == 0 {
		return 0
	}
	return int(math.Pow(2, float64(expo-16383-63))*float64(mant) + 0.5)
}

// --- Ogg (Vorbis / Opus) --------------------------------------------------

func probeOgg(b []byte) Info {
	if i := bytes.Index(b, []byte("OpusHead")); i >= 0 && i+16 <= len(b) {
		return Info{
			Format:     "opus",
			Channels:   int(b[i+9]),
			SampleRate: int(binary.LittleEndian.Uint32(b[i+12 : i+16])),
		}
	}
	if i := bytes.Index(b, []byte("vorbis")); i >= 0 && i+15 <= len(b) {
		return Info{
			Format:     "ogg",
			Channels:   int(b[i+10]),
			SampleRate: int(binary.LittleEndian.Uint32(b[i+11 : i+15])),
		}
	}
	return Info{Format: "ogg"}
}

// --- MP3 / MPEG audio -----------------------------------------------------

func probeMP3(b []byte) (Info, bool) {
	limit := len(b)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i+4 <= limit; i++ {
		if b[i] != 0xFF || b[i+1]&0xE0 != 0xE0 {
			continue
		}
		h := binary.BigEndian.Uint32(b[i : i+4])
		ver := int((h >> 19) & 0x3)   // 0=2.5 1=reserved 2=MPEG2 3=MPEG1
		layer := int((h >> 17) & 0x3) // 0=reserved 1=L3 2=L2 3=L1
		brIdx := int((h >> 12) & 0xF)
		srIdx := int((h >> 10) & 0x3)
		if ver == 1 || layer == 0 || brIdx == 0xF || srIdx == 3 {
			continue
		}
		var sr int
		switch srIdx {
		case 0:
			sr = 44100
		case 1:
			sr = 48000
		case 2:
			sr = 32000
		}
		switch ver {
		case 0: // MPEG 2.5
			sr /= 4
		case 2: // MPEG 2
			sr /= 2
		}
		ch := 2
		if (h>>6)&0x3 == 3 {
			ch = 1
		}
		return Info{
			Format:     "mp3",
			SampleRate: sr,
			Channels:   ch,
			BitRate:    mp3Bitrate(ver, 4-layer, brIdx),
		}, true
	}
	return Info{}, false
}

func mp3Bitrate(ver, layerNum, idx int) int {
	if idx < 1 || idx > 14 {
		return 0
	}
	v1 := map[int][]int{
		1: {32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		2: {32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		3: {32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	}
	v2 := map[int][]int{
		1: {32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		2: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		3: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	}
	table := v1
	if ver == 2 || ver == 0 { // MPEG2 / MPEG2.5
		table = v2
	}
	t := table[layerNum]
	if t == nil {
		return 0
	}
	return t[idx-1]
}

// --- MP4 / M4A (AAC / ALAC) -----------------------------------------------

type box struct {
	typ  string
	off  int64
	size int64
}

func readBoxAt(f *os.File, off int64) (box, bool) {
	var hdr [8]byte
	if _, err := f.ReadAt(hdr[:], off); err != nil {
		return box{}, false
	}
	size := int64(binary.BigEndian.Uint32(hdr[0:4]))
	if size == 1 {
		var big [8]byte
		if _, err := f.ReadAt(big[:], off+8); err != nil {
			return box{}, false
		}
		size = int64(binary.BigEndian.Uint64(big[:]))
	}
	if size < 8 {
		return box{}, false
	}
	return box{typ: string(hdr[4:8]), off: off, size: size}, true
}

func findChild(f *os.File, parent box, want string) (box, bool) {
	end := parent.off + parent.size
	for off := parent.off + 8; off+8 <= end; {
		b, ok := readBoxAt(f, off)
		if !ok {
			return box{}, false
		}
		if b.typ == want {
			return b, true
		}
		if b.size <= 0 {
			return box{}, false
		}
		off += b.size
	}
	return box{}, false
}

func probeMP4(f *os.File) (Info, error) {
	var moov box
	found := false
	for off := int64(0); ; {
		b, ok := readBoxAt(f, off)
		if !ok {
			break
		}
		if b.typ == "moov" {
			moov, found = b, true
			break
		}
		if b.size <= 0 {
			break
		}
		off += b.size
	}
	if !found {
		return Info{Format: "m4a"}, nil
	}
	// Walk every trak; return the first audio sample description we recognize.
	end := moov.off + moov.size
	for off := moov.off + 8; off+8 <= end; {
		b, ok := readBoxAt(f, off)
		if !ok {
			break
		}
		if b.typ == "trak" {
			if info, ok := mp4TrakInfo(f, b); ok {
				return info, nil
			}
		}
		if b.size <= 0 {
			break
		}
		off += b.size
	}
	return Info{Format: "m4a"}, nil
}

func mp4TrakInfo(f *os.File, trak box) (Info, bool) {
	mdia, ok := findChild(f, trak, "mdia")
	if !ok {
		return Info{}, false
	}
	minf, ok := findChild(f, mdia, "minf")
	if !ok {
		return Info{}, false
	}
	stbl, ok := findChild(f, minf, "stbl")
	if !ok {
		return Info{}, false
	}
	stsd, ok := findChild(f, stbl, "stsd")
	if !ok {
		return Info{}, false
	}
	base := stsd.off + 8 + 8 // version/flags + entry count
	var ae [36]byte          // AudioSampleEntry fixed header
	if _, err := f.ReadAt(ae[:], base); err != nil {
		return Info{}, false
	}
	typ := string(ae[4:8])
	ch := int(binary.BigEndian.Uint16(ae[24:26]))
	bps := int(binary.BigEndian.Uint16(ae[26:28]))
	sr := int(binary.BigEndian.Uint32(ae[32:36]) >> 16)
	switch typ {
	case "alac":
		if d := mp4AlacCookie(f, base); d > 0 {
			bps = d
		}
		return Info{Format: "alac", SampleRate: sr, BitDepth: bps, Channels: ch}, true
	case "mp4a":
		return Info{Format: "m4a", SampleRate: sr, Channels: ch}, true
	case "fLaC":
		return Info{Format: "flac", SampleRate: sr, Channels: ch}, true
	case "Opus", "opus":
		return Info{Format: "opus", SampleRate: sr, Channels: ch}, true
	case "ac-3", "ec-3":
		return Info{Format: strings.ToLower(typ), SampleRate: sr, Channels: ch}, true
	}
	return Info{}, false
}

// mp4AlacCookie reads the ALAC magic cookie's bit-depth byte, which is the
// authoritative value (the sample entry's samplesize is often just 16).
func mp4AlacCookie(f *os.File, base int64) int {
	c, ok := readBoxAt(f, base+36)
	if !ok || c.typ != "alac" {
		return 0
	}
	var cookie [24]byte
	if _, err := f.ReadAt(cookie[:], c.off+8+4); err != nil {
		return 0
	}
	return int(cookie[5])
}

// --- helpers --------------------------------------------------------------

// id3v2Size returns the total length of a leading ID3v2 tag (header + body).
func id3v2Size(b []byte) int {
	if len(b) < 10 {
		return -1
	}
	if b[6]&0x80 != 0 || b[7]&0x80 != 0 || b[8]&0x80 != 0 || b[9]&0x80 != 0 {
		return -1 // invalid syncsafe size
	}
	size := int(b[6])<<21 | int(b[7])<<14 | int(b[8])<<7 | int(b[9])
	return 10 + size
}

// isASF reports whether b begins with the ASF (WMA) header object GUID.
func isASF(b []byte) bool {
	guid := []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	return len(b) >= len(guid) && bytes.Equal(b[:len(guid)], guid)
}

// Suffix returns the conventional Subsonic file suffix for the format.
func (i Info) Suffix() string {
	switch i.Format {
	case "flac":
		return "flac"
	case "mp3":
		return "mp3"
	case "wav":
		return "wav"
	case "aiff":
		return "aiff"
	case "ogg":
		return "ogg"
	case "opus":
		return "opus"
	case "m4a", "alac":
		return "m4a"
	case "aac":
		return "aac"
	case "wma":
		return "wma"
	}
	return ""
}

// ContentType returns the MIME type for the format.
func (i Info) ContentType() string {
	switch i.Format {
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "aiff":
		return "audio/aiff"
	case "ogg", "opus":
		return "audio/ogg"
	case "m4a", "alac":
		return "audio/mp4"
	case "aac":
		return "audio/aac"
	case "wma":
		return "audio/x-ms-wma"
	}
	return ""
}
