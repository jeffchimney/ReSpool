package media

import (
	"bytes"
	"strings"
)

// Sniff reports whether head, the first bytes of a file, looks like the
// format its extension claims. It covers the audio formats ReSpool serves and
// the JPEG and PNG images it uses as covers.
func Sniff(ext string, head []byte) bool {
	has := func(prefix string) bool { return bytes.HasPrefix(head, []byte(prefix)) }
	frameSync := len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0
	switch strings.ToLower(ext) {
	case ".mp3":
		return has("ID3") || frameSync
	case ".aac":
		return has("ID3") || has("ADIF") || (len(head) >= 2 && head[0] == 0xFF && head[1]&0xF6 == 0xF0)
	case ".m4a", ".m4b":
		return len(head) >= 8 && string(head[4:8]) == "ftyp"
	case ".flac":
		return has("fLaC") || has("ID3")
	case ".ogg", ".oga", ".opus":
		return has("OggS")
	case ".wav":
		return has("RIFF") && len(head) >= 12 && string(head[8:12]) == "WAVE"
	case ".jpg", ".jpeg":
		return has("\xFF\xD8\xFF")
	case ".png":
		return has("\x89PNG\r\n\x1a\n")
	}
	return false
}
