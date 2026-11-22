// Package media reads the metadata ReSpool needs from audio files: tags,
// embedded cover art, duration and chapter markers.
package media

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhowden/tag"
)

// Info is everything ReSpool knows about a single audio file.
type Info struct {
	Title       string
	Album       string
	Artist      string
	AlbumArtist string
	Comment     string
	Description string // long description (MP4 desc/ldes atoms)
	Track       int
	Disc        int
	Year        int

	HasPicture  bool
	PictureMIME string

	Duration time.Duration // zero when unknown
	Chapters []Chapter
}

// Chapter is a named position inside a file.
type Chapter struct {
	Start time.Duration
	Title string
}

var mimeTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/x-m4a",
	".m4b":  "audio/x-m4a",
	".aac":  "audio/aac",
	".flac": "audio/flac",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
}

// MIMEType returns the content type ReSpool serves for a file extension.
func MIMEType(ext string) (string, bool) {
	t, ok := mimeTypes[strings.ToLower(ext)]
	return t, ok
}

// IsAudio reports whether a file name has an extension ReSpool serves.
func IsAudio(name string) bool {
	_, ok := MIMEType(filepath.Ext(name))
	return ok
}

// Probe reads tags, duration and chapters from the file at path. A file
// without tags is not an error: the returned Info is simply sparse.
func Probe(path string) (info Info, err error) {
	defer func() {
		// The tag parser has panicked on malformed files in the past; one
		// bad file must not take down a whole library scan.
		if r := recover(); r != nil {
			err = fmt.Errorf("probe %s: panic: %v", path, r)
		}
	}()

	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return info, err
	}

	if m, terr := tag.ReadFrom(f); terr == nil {
		info.Title = clean(m.Title())
		info.Album = clean(m.Album())
		info.Artist = clean(m.Artist())
		info.AlbumArtist = clean(m.AlbumArtist())
		info.Comment = clean(m.Comment())
		info.Track, _ = m.Track()
		info.Disc, _ = m.Disc()
		info.Year = m.Year()
		if p := m.Picture(); p != nil && len(p.Data) > 0 {
			info.HasPicture = true
			info.PictureMIME = pictureMIME(p)
		}
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		info.Duration, err = mp3Duration(f, st.Size())
	case ".m4a", ".m4b":
		var mi mp4Info
		mi, err = probeMP4(f, st.Size())
		info.Duration, info.Chapters = mi.duration, mi.chapters
		if mi.description != "" {
			info.Description = mi.description
		}
	case ".flac":
		info.Duration, err = flacDuration(f)
	}
	if err != nil {
		// Duration is nice to have; tags alone still make a usable episode.
		err = fmt.Errorf("probe %s: %w", path, err)
	}
	return info, err
}

// Picture returns the cover art embedded in an audio file.
func Picture(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil, "", err
	}
	p := m.Picture()
	if p == nil || len(p.Data) == 0 {
		return nil, "", fmt.Errorf("%s has no embedded picture", path)
	}
	return p.Data, pictureMIME(p), nil
}

func pictureMIME(p *tag.Picture) string {
	switch t := strings.ToLower(p.MIMEType); {
	case strings.Contains(t, "png"):
		return "image/png"
	case strings.Contains(t, "jp"):
		return "image/jpeg"
	}
	switch strings.ToLower(p.Ext) {
	case "png":
		return "image/png"
	}
	// Sniff the bytes: some taggers leave the MIME type empty.
	if len(p.Data) > 4 && string(p.Data[1:4]) == "PNG" {
		return "image/png"
	}
	return "image/jpeg"
}

func clean(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
}

// skipID3v2 returns the offset of the first byte after any ID3v2 tags at the
// start of r.
func skipID3v2(r io.ReaderAt) int64 {
	var off int64
	hdr := make([]byte, 10)
	for {
		if _, err := r.ReadAt(hdr, off); err != nil || string(hdr[:3]) != "ID3" {
			return off
		}
		size := int64(hdr[6]&0x7f)<<21 | int64(hdr[7]&0x7f)<<14 | int64(hdr[8]&0x7f)<<7 | int64(hdr[9]&0x7f)
		off += 10 + size
		if hdr[5]&0x10 != 0 { // footer present
			off += 10
		}
	}
}

func seconds(f float64) time.Duration {
	return time.Duration(f * float64(time.Second))
}
