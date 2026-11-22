package library

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// naturalLess compares strings case-insensitively, treating runs of digits
// as numbers so "Chapter 2" sorts before "Chapter 10".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			na, ra := digitRun(a)
			nb, rb := digitRun(b)
			ta, tb := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(ta) != len(tb) {
				return len(ta) < len(tb)
			}
			if ta != tb {
				return ta < tb
			}
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRun(s string) (run, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

var discRe = regexp.MustCompile(`(?i)^(?:cd|dis[ck])[\s._-]*(\d{1,3})$`)

// discNumber recognises folders like "CD1", "Disc 2" or "disk_03", whose
// files belong to the parent folder's book.
func discNumber(name string) (int, bool) {
	m := discRe.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

var stripMarks = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// slugify turns a name into a short, URL-safe slug ("Les Misérables" →
// "les-miserables").
func slugify(s string) string {
	if t, _, err := transform.String(stripMarks, s); err == nil {
		s = t
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if b.Len() >= 48 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortHash(s string, n int) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// feedID is derived from the feed's path so it survives restarts and
// rescans: a readable slug plus a hash for uniqueness.
func feedID(key, name string) string {
	h := shortHash("feed:"+key, 8)
	if s := slugify(name); s != "" {
		return s + "-" + h
	}
	return h
}

func episodeID(rel string) string {
	return shortHash("episode:"+rel, 12)
}

// skipName reports whether a file or folder should be ignored: hidden files
// (including macOS "._" resource forks) and NAS housekeeping folders.
func skipName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "@eaDir", "#recycle", "#snapshot", "$RECYCLE.BIN", "System Volume Information", "lost+found":
		return true
	}
	return false
}
