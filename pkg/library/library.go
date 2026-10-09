package library

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Audio extensions indexed and served.
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true,
	".opus": true, ".wav": true, ".aac": true, ".wma": true,
}

// Library is a read-only index over a local music folder (e.g. Lidarr's
// completed-download output). It lets the server play files Lidarr already
// fetched without involving AllDebrid at all. The index is a flat file list
// guarded by sync.RWMutex; refresh it with Scan (initial + periodic).
type Library struct {
	dir string

	mu    sync.RWMutex
	files []fileEntry
}

type fileEntry struct {
	path  string // absolute path
	lower string // lowercased full path for matching
	flac  bool
}

// New builds a Library over dir, which must exist. An empty dir disables
// the tier — main passes nil instead of calling New.
func New(dir string) (*Library, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("library: empty dir")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("library: not a directory: " + dir)
	}
	l := &Library{dir: dir}
	if err := l.Scan(); err != nil {
		return nil, err
	}
	return l, nil
}

// Scan (re)builds the index. Unreadable entries are skipped, never fatal.
func (l *Library) Scan() error {
	var files []fileEntry
	err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !audioExts[ext] {
			return nil
		}
		files = append(files, fileEntry{
			path:  p,
			lower: strings.ToLower(p),
			flac:  ext == ".flac",
		})
		return nil
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.files = files
	l.mu.Unlock()
	return nil
}

// Lookup returns the best local file for a track. The title must appear in
// the path (Lidarr names files "01 - Title.ext"); artist/album containment
// and lossless format rank competing matches.
func (l *Library) Lookup(artist, album, title string) (string, bool) {
	wantT := norm(title)
	if wantT == "" {
		return "", false
	}
	wantA, wantB := norm(artist), norm(album)

	l.mu.RLock()
	defer l.mu.RUnlock()
	best := -1
	path := ""
	for _, f := range l.files {
		normPath := strings.Join(strings.Fields(
			strings.NewReplacer("_", " ", ".", " ", "-", " ").Replace(f.lower)), " ")
		if !strings.Contains(normPath, wantT) {
			continue
		}
		score := 5
		if wantA != "" && strings.Contains(normPath, wantA) {
			score += 2
		}
		if wantB != "" && strings.Contains(normPath, wantB) {
			score += 3
		}
		if f.flac {
			score += 2
		}
		if score > best {
			best = score
			path = f.path
		}
	}
	if best < 0 {
		return "", false
	}
	return path, true
}

// Stats returns the indexed file count. Sizes are deliberately not tracked
// so the index stays tiny (a few dozen bytes per file).
func (l *Library) Stats() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.files)
}

func norm(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer("_", " ", ".", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ")
	s = r.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
