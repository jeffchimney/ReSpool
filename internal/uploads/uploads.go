// Package uploads stores audio files added through ReSpool's web page. Each
// upload becomes a book folder in the uploads folder (or adds to one), and the
// library scans that folder alongside the media folder.
package uploads

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/media"
)

// staging holds uploads while they arrive. It's hidden, so the library
// never scans half-written files.
const staging = ".incoming"

// Store files uploads into book folders under one directory.
type Store struct {
	dir    string
	logger *slog.Logger
	mu     sync.Mutex // serialises filing staged uploads into book folders
}

// New prepares dir for uploads, creating it if needed, and checks that it's
// writable.
func New(dir string, logger *slog.Logger) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	in := filepath.Join(dir, staging)
	// Anything left here is from an upload that was interrupted.
	if err := os.RemoveAll(in); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(in, 0o755); err != nil {
		return nil, err
	}
	probe, err := os.CreateTemp(in, "probe-*")
	if err != nil {
		return nil, fmt.Errorf("uploads folder %s is not writable: %w", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return &Store{dir: dir, logger: logger}, nil
}

// Dir is the absolute uploads folder.
func (s *Store) Dir() string { return s.dir }

// Holds reports whether dir is a book folder inside the uploads folder, as
// opposed to the uploads folder itself or somewhere else.
func (s *Store) Holds(dir string) bool {
	rel, err := filepath.Rel(s.dir, dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// UserError is a problem with what was uploaded, worded for the person who
// uploaded it.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{fmt.Sprintf(format, a...)} }

// Result says where an upload ended up.
type Result struct {
	Dir   string   // the book folder the files were filed into
	Files []string // the stored names of the audio files
	Cover bool     // whether an image was stored as the book's cover
}

type staged struct {
	path string // in the staging folder
	name string // cleaned original name
}

// Receive reads a multipart upload. Its form fields are "files" (audio, plus
// optionally one JPEG or PNG for the cover), "title" and "author" for a new
// book, or "into": the feed ID of an uploaded book to add the files to,
// which resolve turns into that book's folder.
//
// Every file is checked against its extension's format before anything is
// filed, so a rejected upload leaves nothing behind.
func (s *Store) Receive(mr *multipart.Reader, resolve func(feedID string) (dir string, ok bool)) (Result, error) {
	stage, err := os.MkdirTemp(filepath.Join(s.dir, staging), "upload-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(stage)

	var title, author, into string
	var audio []staged
	var cover *staged
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Result{}, fmt.Errorf("reading upload: %w", err)
		}
		if part.FileName() == "" {
			v, err := io.ReadAll(io.LimitReader(part, 1024))
			if err != nil {
				return Result{}, fmt.Errorf("reading upload: %w", err)
			}
			switch part.FormName() {
			case "title":
				title = strings.TrimSpace(string(v))
			case "author":
				author = strings.TrimSpace(string(v))
			case "into":
				into = strings.TrimSpace(string(v))
			}
			continue
		}
		f, image, err := stageFile(stage, n, part)
		if err != nil {
			return Result{}, err
		}
		switch {
		case !image:
			audio = append(audio, f)
		case cover == nil:
			cover = &f
		}
	}
	// A new book needs audio; an existing one can just get a new cover.
	if len(audio) == 0 && (into == "" || cover == nil) {
		return Result{}, userErr("Choose at least one audio file.")
	}

	if into != "" {
		dir, ok := resolve(into)
		if !ok || !s.Holds(dir) {
			return Result{}, userErr("Files can only be added to shows that were uploaded to ReSpool.")
		}
		return s.file(dir, audio, cover, library.Overrides{})
	}
	// What was typed is kept as given, not as cleaned up for the folder name,
	// and wins over the tags of files added later.
	typed := library.Overrides{Title: title, Author: author}
	return s.file(s.bookDir(title, author, audio), audio, cover, typed)
}

// stageFile streams one uploaded file into the staging folder after checking
// that its first bytes match its extension.
func stageFile(stage string, n int, part *multipart.Part) (staged, bool, error) {
	orig := part.FileName()
	name := fileName(orig)
	ext := strings.ToLower(path.Ext(name))
	image := ext == ".jpg" || ext == ".jpeg" || ext == ".png"
	if !image && !media.IsAudio(name) {
		return staged{}, false, userErr("%s isn't a file ReSpool can serve. Use MP3, M4A, M4B, AAC, FLAC, Ogg, Opus or WAV audio, or a JPEG or PNG cover.", orig)
	}

	head := make([]byte, 512)
	got, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return staged{}, false, fmt.Errorf("reading upload: %w", err)
	}
	head = head[:got]
	if len(head) == 0 {
		return staged{}, false, userErr("%s is empty.", orig)
	}
	if !media.Sniff(ext, head) {
		return staged{}, false, userErr("%s doesn't look like real %s data.", orig, strings.ToUpper(strings.TrimPrefix(ext, ".")))
	}

	p := filepath.Join(stage, fmt.Sprintf("%04d%s", n, ext))
	out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return staged{}, false, err
	}
	_, err = out.Write(head)
	if err == nil {
		_, err = io.Copy(out, part)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return staged{}, false, fmt.Errorf("saving %s: %w", orig, err)
	}
	return staged{path: p, name: name}, image, nil
}

// bookDir picks the folder for a new book: Author/Title, taking whatever
// wasn't typed in from the first file's tags.
func (s *Store) bookDir(title, author string, audio []staged) string {
	if title == "" || author == "" {
		info, _ := media.Probe(audio[0].path)
		if author == "" {
			author = firstNonEmpty(info.AlbumArtist, info.Artist)
		}
		if title == "" {
			title = info.Album
		}
		if title == "" && len(audio) == 1 {
			title = firstNonEmpty(info.Title, stem(audio[0].name))
		}
		if title == "" {
			title = sharedTitle(audio)
		}
		if title == "" {
			title = "Upload " + time.Now().Format("2006-01-02 15.04")
		}
	}
	dir := s.dir
	if a := folderName(author); a != "" {
		dir = filepath.Join(dir, a)
	}
	t := folderName(title)
	if t == "" {
		t = "Untitled"
	}
	return filepath.Join(dir, t)
}

// file moves staged files into dir. Names that are already taken get a
// number; an uploaded image replaces the book's cover; typed details are
// saved in the book's overrides file.
func (s *Store) file(dir string, audio []staged, cover *staged, typed library.Overrides) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	res := Result{Dir: dir}
	for _, f := range audio {
		dst := freeName(dir, f.name)
		if err := os.Rename(f.path, dst); err != nil {
			return res, err
		}
		res.Files = append(res.Files, filepath.Base(dst))
	}
	if cover != nil {
		ext := strings.ToLower(path.Ext(cover.name))
		if ext == ".jpeg" {
			ext = ".jpg"
		}
		for _, old := range []string{"cover.jpg", "cover.jpeg", "cover.png"} {
			os.Remove(filepath.Join(dir, old))
		}
		if err := os.Rename(cover.path, filepath.Join(dir, "cover"+ext)); err != nil {
			return res, err
		}
		res.Cover = true
	}
	if typed != (library.Overrides{}) {
		if err := library.WriteOverrides(dir, typed); err != nil {
			return res, err
		}
	}
	s.logger.Info("upload filed", "dir", dir, "files", len(res.Files), "cover", res.Cover)
	return res, nil
}

// freeName returns dir/name, or dir/"name (2).ext" and so on if that's taken.
func freeName(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := path.Ext(name)
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); err != nil {
			return p // free (or unreadable, which the rename will report)
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem(name), i, ext))
	}
}

// fileName makes an uploaded file's name safe to store: no folders, no
// characters that file systems or SMB shares reject, and no leading dot.
func fileName(orig string) string {
	base := path.Base(strings.ReplaceAll(orig, `\`, "/"))
	ext := strings.ToLower(path.Ext(base))
	s := folderName(strings.TrimSuffix(base, path.Ext(base)))
	if s == "" {
		s = "audio"
	}
	return s + ext
}

// folderName cleans a title or author for use as a folder name.
func folderName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
		case r == ':':
			b.WriteString(" - ")
		case strings.ContainsRune(`/\*?"<>|`, r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(strings.Join(strings.Fields(b.String()), " "), ". -")
	for len(out) > 120 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return strings.TrimRight(out, ". ")
}

var partWords = map[string]bool{"part": true, "pt": true, "chapter": true, "ch": true, "track": true, "disc": true, "disk": true, "cd": true}

// sharedTitle guesses a book title from what its files' names have in
// common: "Dune Part 1.mp3" and "Dune Part 2.mp3" give "Dune".
func sharedTitle(audio []staged) string {
	if len(audio) < 2 {
		return ""
	}
	prefix := stem(audio[0].name)
	for _, f := range audio[1:] {
		name := stem(f.name)
		i := 0
		for i < len(prefix) && i < len(name) && prefix[i] == name[i] {
			i++
		}
		prefix = prefix[:i]
	}
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	words := strings.FieldsFunc(prefix, func(r rune) bool { return r == ' ' || r == '_' || r == '-' || r == '.' })
	for len(words) > 0 {
		last := strings.ToLower(words[len(words)-1])
		if partWords[last] || strings.Trim(last, "0123456789") == "" {
			words = words[:len(words)-1]
			continue
		}
		break
	}
	title := strings.Join(words, " ")
	if len([]rune(title)) < 3 {
		return ""
	}
	return title
}

func stem(name string) string { return strings.TrimSuffix(name, path.Ext(name)) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Edit replaces the title, author and description saved for an uploaded
// book. Empty fields go back to the files' tags. The folder isn't renamed,
// so the book's feed URL stays the same.
func (s *Store) Edit(dir string, o library.Overrides) error {
	if !s.Holds(dir) {
		return userErr("Only shows uploaded to ReSpool can be edited here.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return library.SaveOverrides(dir, o)
}

// Delete permanently removes an uploaded book's folder and everything in it.
func (s *Store) Delete(dir string) error {
	if !s.Holds(dir) {
		return userErr("Only shows uploaded to ReSpool can be deleted here.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.pruneParents(dir)
	s.logger.Info("uploaded show deleted", "dir", dir)
	return nil
}

// DeleteFile permanently removes one audio file from an uploaded book. If
// that was the book's last audio file, the whole book goes, and bookGone is
// true.
func (s *Store) DeleteFile(dir, file string) (bookGone bool, err error) {
	rel, err := filepath.Rel(dir, file)
	if !s.Holds(dir) || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, userErr("Only files in shows uploaded to ReSpool can be removed here.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	s.logger.Info("uploaded file deleted", "file", file)
	if hasAudio(dir) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	s.pruneParents(dir)
	return true, nil
}

// pruneParents removes the now-empty author folders above a deleted book,
// stopping at the uploads folder.
func (s *Store) pruneParents(dir string) {
	for p := filepath.Dir(dir); s.Holds(p); p = filepath.Dir(p) {
		if os.Remove(p) != nil { // fails unless empty
			return
		}
	}
}

func hasAudio(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && media.IsAudio(d.Name()) && !strings.HasPrefix(d.Name(), ".") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
