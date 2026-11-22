package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/uploads"
)

func newUploadServer(t *testing.T, limit int64) (*Server, *uploads.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := uploads.New(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	lib := library.New(logger, library.Root{Dir: t.TempDir()}, library.Root{Dir: store.Dir(), Prefix: "@uploads/"})
	if _, err := lib.Scan(); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Library: lib, Token: token, Logger: logger, Uploads: store, UploadLimit: limit})
	return s, store
}

func postUpload(t *testing.T, s *Server, header bool, fields map[string]string, files ...string) (*httptest.ResponseRecorder, uploadResponse) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join("../media/testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		fw, _ := w.CreateFormFile("files", name)
		fw.Write(data)
	}
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "http://nas.local:8080/"+token+"/upload", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	if header {
		r.Header.Set("X-Requested-With", "ReSpool")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	var res uploadResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func TestUpload(t *testing.T) {
	s, store := newUploadServer(t, DefaultUploadLimit)

	rec, res := postUpload(t, s, true, nil, "tagged.mp3")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if res.Title != "Test Book" || res.Files != 1 || !strings.HasPrefix(res.FeedURL, "http://nas.local:8080/"+token+"/feeds/") {
		t.Errorf("response = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "Test Author", "Test Book", "tagged.mp3")); err != nil {
		t.Errorf("uploaded file not stored: %v", err)
	}

	// The new book is on the page straight away, and can take more files.
	feedID := strings.TrimSuffix(res.FeedURL[strings.LastIndex(res.FeedURL, "/")+1:], ".xml")
	html := body(t, get(t, s, "http://nas.local:8080/"+token+"/"))
	for _, want := range []string{"Test Book", `id="add-audio"`, `<option value="` + feedID + `">Test Book</option>`} {
		if !strings.Contains(html, want) {
			t.Errorf("index is missing %q", want)
		}
	}
	if feed := get(t, s, res.FeedURL); feed.StatusCode != http.StatusOK {
		t.Errorf("new feed = %d", feed.StatusCode)
	}

	rec, res = postUpload(t, s, true, map[string]string{"into": feedID}, "vbr.mp3")
	if rec.Code != http.StatusOK || res.Title != "Test Book" || res.Files != 1 {
		t.Errorf("adding to the book = %d %+v", rec.Code, res)
	}

	// Typed details win over the files' tags.
	rec, res = postUpload(t, s, true, map[string]string{"title": "Typed: Title", "author": "Typed Author"}, "tagged.mp3")
	if rec.Code != http.StatusOK || res.Title != "Typed: Title" {
		t.Fatalf("upload with a typed title = %d %+v", rec.Code, res)
	}
	if xml := body(t, get(t, s, res.FeedURL)); !strings.Contains(xml, "<itunes:author>Typed Author</itunes:author>") {
		t.Error("feed doesn't use the typed author")
	}
}

func TestUploadRejections(t *testing.T) {
	s, _ := newUploadServer(t, 4096)

	// Another site can't add the custom header without a CORS preflight.
	if rec, _ := postUpload(t, s, false, nil, "tagged.mp3"); rec.Code != http.StatusForbidden {
		t.Errorf("upload without the ReSpool header = %d, want 403", rec.Code)
	}
	rec, res := postUpload(t, s, true, nil, "tagged.mp3") // ~13 KB against a 4 KB limit
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(res.Error, "4 KB") {
		t.Errorf("oversized upload = %d %q, want 413", rec.Code, res.Error)
	}
	rec, res = postUpload(t, s, true, map[string]string{"title": "No files"})
	if rec.Code != http.StatusBadRequest || res.Error == "" {
		t.Errorf("upload without files = %d %q, want 400 with a message", rec.Code, res.Error)
	}
}

func TestUploadsOff(t *testing.T) {
	s, _ := newServer(t)
	if rec, _ := postUpload(t, s, true, nil, "tagged.mp3"); rec.Code != http.StatusNotFound {
		t.Errorf("upload with uploads off = %d, want 404", rec.Code)
	}
	if html := body(t, get(t, s, "/"+token+"/")); strings.Contains(html, `id="add-audio"`) || strings.Contains(html, "<dialog") {
		t.Error("index offers uploads when they're off")
	}
}

func call(t *testing.T, s *Server, method, target string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, "http://h/"+token+target, strings.NewReader(body))
	r.Header.Set("X-Requested-With", "ReSpool")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestManageUploadedBooks(t *testing.T) {
	s, _ := newUploadServer(t, DefaultUploadLimit)
	_, up := postUpload(t, s, true, map[string]string{"title": "Original"}, "vbr.mp3", "cbr-noxing.mp3")
	id := strings.TrimSuffix(up.FeedURL[strings.LastIndex(up.FeedURL, "/")+1:], ".xml")

	rec, details := call(t, s, http.MethodGet, "/books/"+id, "")
	files, _ := details["files"].([]any)
	if rec.Code != http.StatusOK || details["title"] != "Original" || len(files) != 2 {
		t.Fatalf("details = %d %v", rec.Code, details)
	}

	rec, out := call(t, s, http.MethodPatch, "/books/"+id, `{"title":"Renamed","author":"Someone"}`)
	if rec.Code != http.StatusOK || out["title"] != "Renamed" {
		t.Fatalf("edit = %d %v", rec.Code, out)
	}
	if s.snapshot().Feed(id) == nil {
		t.Fatal("renaming a book changed its feed URL")
	}
	if feed := body(t, get(t, s, up.FeedURL)); !strings.Contains(feed, "<title>Renamed</title>") || !strings.Contains(feed, "Someone") {
		t.Error("feed doesn't show the new title and author")
	}

	first := files[0].(map[string]any)["id"].(string)
	rec, out = call(t, s, http.MethodDelete, "/books/"+id+"/files/"+first, "")
	if rec.Code != http.StatusOK || out["bookDeleted"] == true || len(s.snapshot().Feed(id).Episodes) != 1 {
		t.Fatalf("removing a file = %d %v", rec.Code, out)
	}

	rec, out = call(t, s, http.MethodDelete, "/books/"+id, "")
	if rec.Code != http.StatusOK || out["bookDeleted"] != true || s.snapshot().Feed(id) != nil {
		t.Fatalf("deleting the book = %d %v", rec.Code, out)
	}
}

func TestMediaBooksCantBeManaged(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	media := t.TempDir()
	data, _ := os.ReadFile("../media/testdata/vbr.mp3")
	os.MkdirAll(filepath.Join(media, "Mine"), 0o755)
	os.WriteFile(filepath.Join(media, "Mine", "01.mp3"), data, 0o644)
	store, _ := uploads.New(t.TempDir(), logger)
	lib := library.New(logger, library.Root{Dir: media}, library.Root{Dir: store.Dir(), Prefix: "@uploads/"})
	snap, _ := lib.Scan()
	s := New(Options{Library: lib, Token: token, Logger: logger, Uploads: store})
	id := snap.Feeds[0].ID

	for _, tt := range []struct{ method, target string }{
		{http.MethodGet, "/books/" + id},
		{http.MethodPatch, "/books/" + id},
		{http.MethodDelete, "/books/" + id},
		{http.MethodDelete, "/books/" + id + "/files/" + snap.Feeds[0].Episodes[0].ID},
	} {
		if rec, _ := call(t, s, tt.method, tt.target, `{"title":"x"}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on a media book = %d, want 404", tt.method, tt.target, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(media, "Mine", "01.mp3")); err != nil {
		t.Fatal("a media file was touched")
	}

	// Changes without ReSpool's header are refused before anything else.
	r := httptest.NewRequest(http.MethodDelete, "http://h/"+token+"/books/"+id, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE without the header = %d, want 403", rec.Code)
	}
}
