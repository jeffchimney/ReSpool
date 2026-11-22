package library

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fixtures = "../media/testdata"

// buildLibrary creates a media folder from fixture files. Keys are paths inside the
// folder; values are fixture names, or "" for an empty file.
func buildLibrary(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dst, src := range files {
		p := filepath.Join(root, filepath.FromSlash(dst))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var data []byte
		if src != "" {
			var err error
			if data, err = os.ReadFile(filepath.Join(fixtures, src)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func byPath(t *testing.T, s *Snapshot, p string) *Feed {
	t.Helper()
	for _, f := range s.Feeds {
		if f.Path == p {
			return f
		}
	}
	t.Fatalf("no feed for %q", p)
	return nil
}

func titles(f *Feed) []string {
	var out []string
	for _, e := range f.Episodes {
		out = append(out, e.Title)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScan(t *testing.T) {
	root := buildLibrary(t, map[string]string{
		"Ada Author/Chaptered Book/book.m4b": "chapters.m4b",
		"Big Book/CD1/01 Start.mp3":          "vbr.mp3",
		"Big Book/CD1/02 Next.mp3":           "cbr-noxing.mp3",
		"Big Book/CD 2/01 More.mp3":          "vbr.mp3",
		"Big Book/Cover.PNG":                 "",
		"Series/Book 2/Part 10.mp3":          "vbr.mp3",
		"Series/Book 2/Part 2.mp3":           "vbr.mp3",
		"Series/Book 2/Part 1.mp3":           "vbr.mp3",
		"Series/Book 2/description.txt":      "",
		"Same Titles/01.mp3":                 "tagged.mp3",
		"Same Titles/02.mp3":                 "tagged.mp3",
		"Loose Tagged.mp3":                   "tagged.mp3",
		".hidden/skip.mp3":                   "vbr.mp3",
		"@eaDir/skip.mp3":                    "vbr.mp3",
		"Big Book/._01 Start.mp3":            "",
		"notes.txt":                          "",
	})
	os.WriteFile(filepath.Join(root, "Series/Book 2/description.txt"), []byte("  The second book.\n"), 0o644)

	lib := New(quiet(), Root{Dir: root})
	snap, err := lib.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Feeds) != 5 {
		for _, f := range snap.Feeds {
			t.Logf("feed %q (%s)", f.Title, f.Path)
		}
		t.Fatalf("got %d feeds, want 5", len(snap.Feeds))
	}

	big := byPath(t, snap, "Big Book")
	if want := []string{"01 Start", "02 Next", "01 More"}; !equal(titles(big), want) {
		t.Errorf("Big Book episodes = %q, want %q (disc folders merged in order)", titles(big), want)
	}
	if big.Cover.MIMEType != "image/png" || filepath.Base(big.Cover.Path) != "Cover.PNG" {
		t.Errorf("Big Book cover = %+v", big.Cover)
	}
	for i := 1; i < len(big.Episodes); i++ {
		if !big.Episodes[i].PubDate.After(big.Episodes[i-1].PubDate) {
			t.Errorf("episode %d pubDate %v not after %v", i, big.Episodes[i].PubDate, big.Episodes[i-1].PubDate)
		}
	}
	if last := big.Episodes[len(big.Episodes)-1]; last.PubDate.After(time.Now()) {
		t.Errorf("last episode is dated in the future (%v); apps may hide it", last.PubDate)
	}

	series := byPath(t, snap, "Series/Book 2")
	if want := []string{"Part 1", "Part 2", "Part 10"}; !equal(titles(series), want) {
		t.Errorf("Book 2 episodes = %q, want natural order %q", titles(series), want)
	}
	if series.Title != "Book 2" || series.Description != "The second book." {
		t.Errorf("Book 2 = %q / %q", series.Title, series.Description)
	}

	chap := byPath(t, snap, "Ada Author/Chaptered Book")
	if chap.Title != "Chaptered Book" || chap.Author != "Ada Author" || len(chap.Episodes) != 1 {
		t.Errorf("Chaptered Book = %q by %q, %d episodes", chap.Title, chap.Author, len(chap.Episodes))
	}
	if len(chap.Episodes[0].Chapters) != 3 || chap.Description != "A tiny book used in tests." {
		t.Errorf("Chaptered Book chapters = %+v, description %q", chap.Episodes[0].Chapters, chap.Description)
	}

	same := byPath(t, snap, "Same Titles")
	if want := []string{"01", "02"}; !equal(titles(same), want) {
		t.Errorf("identical title tags should fall back to filenames: got %q", titles(same))
	}

	loose := byPath(t, snap, "Loose Tagged.mp3")
	if loose.Title != "Test Book" || !loose.Cover.Embedded || loose.Author != "Test Author" {
		t.Errorf("loose file feed = %q by %q, cover %+v", loose.Title, loose.Author, loose.Cover)
	}

	// IDs must survive a rescan so subscriptions keep working.
	again, err := lib.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range snap.Feeds {
		g := again.Feed(f.ID)
		if g == nil || g.Path != f.Path {
			t.Fatalf("feed %q has a different ID after rescan", f.Path)
		}
		for _, e := range f.Episodes {
			if g.Episode(e.ID) == nil {
				t.Errorf("episode %q has a different ID after rescan", e.RelPath)
			}
		}
	}
}

func TestNaturalLess(t *testing.T) {
	ordered := []string{"Chapter 1", "chapter 2", "Chapter 10", "Chapter 10a", "Chapter 11", "Epilogue"}
	for i := 0; i+1 < len(ordered); i++ {
		if !naturalLess(ordered[i], ordered[i+1]) || naturalLess(ordered[i+1], ordered[i]) {
			t.Errorf("want %q < %q", ordered[i], ordered[i+1])
		}
	}
}

func TestDiscNumber(t *testing.T) {
	for name, want := range map[string]int{"CD1": 1, "cd 02": 2, "Disc 3": 3, "disk_4": 4, "Disc-12": 12} {
		if got, ok := discNumber(name); !ok || got != want {
			t.Errorf("discNumber(%q) = %d, %v", name, got, ok)
		}
	}
	for _, name := range []string{"Book 1", "CD", "Discography"} {
		if _, ok := discNumber(name); ok {
			t.Errorf("discNumber(%q) matched", name)
		}
	}
}

func TestMultipleRoots(t *testing.T) {
	media := buildLibrary(t, map[string]string{"Book/01.mp3": "vbr.mp3", "Loose.mp3": "vbr.mp3"})
	uploads := buildLibrary(t, map[string]string{"Book/01.mp3": "vbr.mp3", "Loose.mp3": "vbr.mp3"})

	only, err := New(quiet(), Root{Dir: media}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	both, err := New(quiet(), Root{Dir: media}, Root{Dir: uploads, Prefix: "@uploads/"}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(both.Feeds) != 4 {
		t.Fatalf("got %d feeds from two roots with the same layout, want 4", len(both.Feeds))
	}
	// Adding a root must not change the IDs of feeds people already follow.
	for _, f := range only.Feeds {
		if g := both.Feed(f.ID); g == nil || g.Dir != f.Dir {
			t.Errorf("feed %q changed ID when a second root was added", f.Path)
		}
	}
	up := byPath(t, both, "@uploads/Book")
	if up.Dir != filepath.Join(uploads, "Book") || up.Title != "Book" {
		t.Errorf("uploaded feed = %q in %q", up.Title, up.Dir)
	}
	if loose := byPath(t, both, "@uploads/Loose.mp3"); loose.Title != "Loose" || loose.Dir != uploads {
		t.Errorf("loose uploaded file = %q in %q", loose.Title, loose.Dir)
	}
}

func TestOverrides(t *testing.T) {
	root := buildLibrary(t, map[string]string{"Book/01.mp3": "tagged.mp3"})
	before, err := New(quiet(), Root{Dir: root}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteOverrides(filepath.Join(root, "Book"), Overrides{Title: "Better Title", Author: "Better Author"}); err != nil {
		t.Fatal(err)
	}
	// Merging keeps the fields already there.
	if err := WriteOverrides(filepath.Join(root, "Book"), Overrides{Description: "Better."}); err != nil {
		t.Fatal(err)
	}
	after, err := New(quiet(), Root{Dir: root}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	f := after.Feeds[0]
	if f.Title != "Better Title" || f.Author != "Better Author" || f.Description != "Better." {
		t.Errorf("feed = %q by %q, %q; want the overrides over the tags", f.Title, f.Author, f.Description)
	}
	if f.ID != before.Feeds[0].ID {
		t.Error("overriding the title changed the feed's ID")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Les Misérables":       "les-miserables",
		"  The Hobbit (1937) ": "the-hobbit-1937",
		"三体":                   "",
		"Dune: Part One!":      "dune-part-one",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
