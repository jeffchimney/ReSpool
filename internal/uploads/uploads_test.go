package uploads

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jeffchimney/respool/internal/library"
)

type part struct {
	field, filename string
	data            []byte
}

func field(name, value string) part { return part{field: name, data: []byte(value)} }

func upload(name string, data []byte) part { return part{field: "files", filename: name, data: data} }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../media/testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func receive(t *testing.T, s *Store, resolve func(string) (string, bool), parts ...part) (Result, error) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename == "" {
			w.WriteField(p.field, string(p.data))
			continue
		}
		fw, err := w.CreateFormFile(p.field, p.filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(p.data)
	}
	w.Close()
	if resolve == nil {
		resolve = func(string) (string, bool) { return "", false }
	}
	return s.Receive(multipart.NewReader(&buf, w.Boundary()), resolve)
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

// stagingEmpty checks that nothing was left behind mid-upload.
func stagingEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Dir(), staging))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging folder still holds %d entries", len(entries))
	}
}

func TestNewBookNamedFromTags(t *testing.T) {
	s := newStore(t)
	res, err := receive(t, s, nil, upload("tagged.mp3", fixture(t, "tagged.mp3")))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Dir(), "Test Author", "Test Book")
	if res.Dir != want || len(res.Files) != 1 || !exists(t, filepath.Join(want, "tagged.mp3")) {
		t.Errorf("result = %+v, want the file in %s", res, want)
	}
	stagingEmpty(t, s)
}

func TestTypedTitleAndCover(t *testing.T) {
	s := newStore(t)
	res, err := receive(t, s, nil,
		field("title", "  My Book: Volume 1 "),
		upload("01.mp3", fixture(t, "vbr.mp3")),
		upload("02.mp3", fixture(t, "cbr-noxing.mp3")),
		upload("Front Art.JPEG", []byte("\xFF\xD8\xFF\xE0 not really a jpeg")),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Dir(), "My Book - Volume 1") // no author typed or tagged
	if res.Dir != want || len(res.Files) != 2 || !res.Cover {
		t.Fatalf("result = %+v, want two files and a cover in %s", res, want)
	}
	if !exists(t, filepath.Join(want, "cover.jpg")) || exists(t, filepath.Join(want, "Front Art.jpeg")) {
		t.Error("the uploaded image should be stored as cover.jpg")
	}
	// The typed title is kept as typed, colon and all, for the feed to use.
	if o := library.ReadOverrides(want); o.Title != "My Book: Volume 1" || o.Author != "" {
		t.Errorf("overrides = %+v", o)
	}
}

func TestTagNamedBooksHaveNoOverrides(t *testing.T) {
	s := newStore(t)
	res, err := receive(t, s, nil, upload("tagged.mp3", fixture(t, "tagged.mp3")))
	if err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(res.Dir, library.OverridesFile)) {
		t.Error("a book named from its tags shouldn't get an overrides file")
	}
}

func TestAddToExistingBook(t *testing.T) {
	s := newStore(t)
	first, err := receive(t, s, nil, upload("tagged.mp3", fixture(t, "tagged.mp3")))
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(id string) (string, bool) { return first.Dir, id == "feed-1" }
	second, err := receive(t, s, resolve,
		field("into", "feed-1"),
		field("title", "ignored when adding to a book"),
		upload("tagged.mp3", fixture(t, "tagged.mp3")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Dir != first.Dir || len(second.Files) != 1 || second.Files[0] != "tagged (2).mp3" {
		t.Errorf("second upload = %+v, want tagged (2).mp3 in %s", second, first.Dir)
	}

	outside := func(string) (string, bool) { return t.TempDir(), true }
	_, err = receive(t, s, outside, field("into", "feed-2"), upload("a.mp3", fixture(t, "vbr.mp3")))
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Errorf("adding to a book outside the uploads folder: err = %v, want a UserError", err)
	}
}

func TestRejectedUploadsLeaveNothing(t *testing.T) {
	tests := map[string][]part{
		"text pretending to be mp3": {upload("good.mp3", fixture(t, "vbr.mp3")), upload("notes.mp3", []byte("just some text"))},
		"unsupported type":          {upload("book.pdf", []byte("%PDF-1.7"))},
		"empty file":                {upload("empty.m4b", nil)},
		"no audio":                  {upload("cover.png", []byte("\x89PNG\r\n\x1a\n...."))},
		"nothing":                   {field("title", "Nothing")},
	}
	for name, parts := range tests {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			_, err := receive(t, s, nil, parts...)
			var ue *UserError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want a UserError", err)
			}
			entries, _ := os.ReadDir(s.Dir())
			if len(entries) != 1 { // just .incoming
				t.Errorf("uploads folder has %d entries after a rejected upload", len(entries))
			}
			stagingEmpty(t, s)
		})
	}
}

func TestFileNamesAreSafe(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd.mp3": "passwd.mp3",
		`C:\Users\me\Ch 1.MP3`: "Ch 1.mp3",
		".hidden.mp3":          "hidden.mp3",
		"what?*.mp3":           "what.mp3",
		"...mp3":               "audio.mp3",
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200)
	if got := folderName(long); len(got) > 120 || !utf8.ValidString(got) {
		t.Errorf("folderName of a long name = %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
	if got := folderName("AC/DC: Live at Donington "); got != "AC DC - Live at Donington" {
		t.Errorf("folderName = %q", got)
	}
}

func TestSharedTitle(t *testing.T) {
	files := func(names ...string) []staged {
		var out []staged
		for _, n := range names {
			out = append(out, staged{name: n})
		}
		return out
	}
	for want, names := range map[string][]string{
		"Dune":     {"Dune Part 1.mp3", "Dune Part 2.mp3", "Dune Part 10.mp3"},
		"Mistborn": {"Mistborn_01.mp3", "Mistborn_02.mp3"},
		"":         {"01 Intro.mp3", "02 Storm.mp3"},
	} {
		if got := sharedTitle(files(names...)); got != want {
			t.Errorf("sharedTitle(%q) = %q, want %q", names, got, want)
		}
	}
}

func TestHolds(t *testing.T) {
	s := newStore(t)
	for dir, want := range map[string]bool{
		filepath.Join(s.Dir(), "Book"):           true,
		filepath.Join(s.Dir(), "Author", "Book"): true,
		filepath.Join(s.Dir(), "..folder"):       true,
		s.Dir():                                  false,
		filepath.Dir(s.Dir()):                    false,
		filepath.Join(s.Dir(), "..", "x"):        false,
	} {
		if got := s.Holds(dir); got != want {
			t.Errorf("Holds(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestNewClearsInterruptedUploads(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, staging, "upload-123")
	os.MkdirAll(leftover, 0o755)
	os.WriteFile(filepath.Join(leftover, "0001.mp3"), []byte("partial"), 0o644)
	if _, err := New(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if exists(t, leftover) {
		t.Error("New should clear uploads left over from a crash")
	}
}

func TestEditDeleteAndRemove(t *testing.T) {
	s := newStore(t)
	book, err := receive(t, s, nil,
		field("title", "Two Parts"), field("author", "Someone"),
		upload("01.mp3", fixture(t, "vbr.mp3")), upload("02.mp3", fixture(t, "vbr.mp3")))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Edit(book.Dir, library.Overrides{Title: "Renamed", Description: "Now with a blurb."}); err != nil {
		t.Fatal(err)
	}
	if o := library.ReadOverrides(book.Dir); o.Title != "Renamed" || o.Author != "" || o.Description != "Now with a blurb." {
		t.Errorf("after edit, overrides = %+v (an emptied author should go back to the tags)", o)
	}

	// A cover alone can be added to an existing book.
	resolve := func(string) (string, bool) { return book.Dir, true }
	if res, err := receive(t, s, resolve, field("into", "x"), upload("c.png", []byte("\x89PNG\r\n\x1a\n...."))); err != nil || !res.Cover {
		t.Errorf("cover-only upload = %+v, %v", res, err)
	}

	gone, err := s.DeleteFile(book.Dir, filepath.Join(book.Dir, "01.mp3"))
	if err != nil || gone || exists(t, filepath.Join(book.Dir, "01.mp3")) {
		t.Fatalf("removing one of two files: gone=%v err=%v", gone, err)
	}
	gone, err = s.DeleteFile(book.Dir, filepath.Join(book.Dir, "02.mp3"))
	if err != nil || !gone || exists(t, book.Dir) {
		t.Fatalf("removing the last file should delete the book: gone=%v err=%v", gone, err)
	}
	if exists(t, filepath.Join(s.Dir(), "Someone")) {
		t.Error("the empty author folder should be removed too")
	}
	if !exists(t, s.Dir()) {
		t.Fatal("the uploads folder itself must survive")
	}

	other, err := receive(t, s, nil, upload("tagged.mp3", fixture(t, "tagged.mp3")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(other.Dir); err != nil || exists(t, other.Dir) {
		t.Errorf("Delete: err=%v, still there=%v", err, exists(t, other.Dir))
	}
}

func TestManagementStaysInsideUploads(t *testing.T) {
	s := newStore(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep.mp3"), fixture(t, "vbr.mp3"), 0o644)
	var ue *UserError
	if err := s.Delete(outside); !errors.As(err, &ue) {
		t.Errorf("Delete outside uploads: err = %v", err)
	}
	if err := s.Delete(s.Dir()); !errors.As(err, &ue) {
		t.Errorf("Delete of the uploads folder itself: err = %v", err)
	}
	if _, err := s.DeleteFile(outside, filepath.Join(outside, "keep.mp3")); !errors.As(err, &ue) {
		t.Errorf("DeleteFile outside uploads: err = %v", err)
	}
	book := filepath.Join(s.Dir(), "Book")
	os.MkdirAll(book, 0o755)
	if _, err := s.DeleteFile(book, filepath.Join(outside, "keep.mp3")); !errors.As(err, &ue) {
		t.Errorf("DeleteFile of a path outside the book: err = %v", err)
	}
	if err := s.Edit(outside, library.Overrides{Title: "x"}); !errors.As(err, &ue) {
		t.Errorf("Edit outside uploads: err = %v", err)
	}
	if !exists(t, filepath.Join(outside, "keep.mp3")) {
		t.Fatal("a file outside the uploads folder was deleted")
	}
}
