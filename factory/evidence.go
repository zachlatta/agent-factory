package factory

import (
	"bytes"
	"encoding/binary"
	"errors"
	stdimage "image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// Evidence names untracked files, never URLs or paths supplied by a browser.
type Evidence struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

var evidenceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}\.(png|jpg|jpeg|webp)$`)

func validEvidenceName(name string) bool { return evidenceName.MatchString(name) }

// parseEvidence reads only the contiguous attachment header immediately after the opening
// overview. Non-filenames remain report text, including before/after copy examples.
func parseEvidence(text string) (string, Evidence) {
	trimmed := strings.TrimSpace(text)
	if !overviewKey.MatchString(strings.SplitN(trimmed, "\n", 2)[0]) {
		return text, Evidence{}
	}
	rest, overview := ParseOverview(trimmed)
	if overview == nil || rest == "" || !strings.HasSuffix(trimmed, rest) {
		return text, Evidence{}
	}
	prefix := trimmed[:len(trimmed)-len(rest)]
	var ev Evidence
	var kept []string
	lines := strings.Split(rest, "\n")
	i := 0
	for ; i < len(lines); i++ {
		key, name, ok := strings.Cut(lines[i], ":")
		if !ok || (key != "BEFORE" && key != "AFTER") {
			break
		}
		name = strings.TrimSpace(name)
		if !validEvidenceName(name) {
			kept = append(kept, lines[i])
			continue
		}
		if key == "BEFORE" && ev.Before == "" {
			ev.Before = name
		}
		if key == "AFTER" && ev.After == "" {
			ev.After = name
		}
	}
	if ev == (Evidence{}) {
		return text, ev
	}
	kept = append(kept, lines[i:]...)
	return prefix + strings.TrimSpace(strings.Join(kept, "\n")), ev
}

// readEvidence confines reads to the evidence directory and rejects substituted directories,
// symlinks, special files and images that cannot be safely decoded.
func readEvidence(cwd, name string) ([]byte, string, error) {
	if cwd == "" || !validEvidenceName(name) {
		return nil, "", errors.New("invalid evidence")
	}
	work, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, "", err
	}
	defer work.Close()
	info, err := work.Lstat(".factory-evidence")
	if err != nil || !info.IsDir() {
		return nil, "", errors.New("missing evidence directory")
	}
	root, err := work.OpenRoot(".factory-evidence")
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, "", errors.New("changed evidence directory")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 5<<20 {
		return nil, "", errors.New("invalid evidence file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (5<<20)+1))
	if err != nil || len(data) > 5<<20 {
		return nil, "", errors.New("oversized evidence")
	}
	if strings.HasSuffix(name, ".webp") {
		// a short video of a UI working: the standard library has no WebP decoder, so check the container
		if !validWebp(data) {
			return nil, "", errors.New("invalid evidence video")
		}
		return data, "image/webp", nil
	}
	cfg, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return nil, "", errors.New("invalid evidence image")
	}
	if _, _, err = stdimage.Decode(bytes.NewReader(data)); err != nil {
		return nil, "", err
	}
	return data, "image/" + format, nil
}

// validWebp checks a RIFF WEBP container whose declared size fits the file and whose canvas, read from its
// first chunk, is within the same pixel limit as other evidence.
func validWebp(data []byte) bool {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" ||
		int64(binary.LittleEndian.Uint32(data[4:8]))+8 != int64(len(data)) {
		return false
	}
	var w, h int64
	switch string(data[12:16]) {
	case "VP8X": // canvas size minus one, 24 bits each
		w = int64(data[24]) | int64(data[25])<<8 | int64(data[26])<<16 + 1
		h = int64(data[27]) | int64(data[28])<<8 | int64(data[29])<<16 + 1
	case "VP8 ": // a key frame: start code, then 14-bit sizes
		if data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return false
		}
		w = int64(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		h = int64(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
	case "VP8L": // signature, then 14-bit sizes minus one
		if data[20] != 0x2f {
			return false
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		w = int64(bits&0x3fff) + 1
		h = int64(bits>>14&0x3fff) + 1
	default:
		return false
	}
	return w > 0 && h > 0 && w*h <= 20_000_000
}

func (f *Factory) handleEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	c, ok := f.card(r.PathValue("id"))
	name := ""
	if ok && c.Ask != nil {
		switch r.PathValue("slot") {
		case "before":
			name = c.Ask.Evidence.Before
		case "after":
			name = c.Ask.Evidence.After
		}
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	data, media, err := readEvidence(c.Cwd, name)
	if err != nil && len(c.Replaces) > 0 {
		lin := cardLineage(f.byID(), c.ID)
		for i := len(lin) - 2; i >= 0 && err != nil; i-- {
			if lin[i].Cwd != "" && lin[i].Cwd != c.Cwd {
				data, media, err = readEvidence(lin[i].Cwd, name)
			}
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", media)
	w.Write(data)
}
