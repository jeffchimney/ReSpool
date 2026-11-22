package server

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jeffchimney/respool/internal/library"
)

const token = "s3cret"

func newServer(t *testing.T) (*Server, *library.Snapshot) {
	t.Helper()
	root := t.TempDir()
	for dst, src := range map[string]string{
		"Chaptered Book/book.m4b": "chapters.m4b",
		"Two Parts/01.mp3":        "vbr.mp3",
		"Two Parts/02.mp3":        "cbr-noxing.mp3",
	} {
		data, err := os.ReadFile(filepath.Join("../media/testdata", src))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, dst)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	lib := library.New(logger, library.Root{Dir: root})
	snap, err := lib.Scan()
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Library: lib, Token: token, Language: "en", Version: "test", Logger: logger}), snap
}

func get(t *testing.T, s *Server, target string, header ...string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func feedFor(t *testing.T, snap *library.Snapshot, title string) *library.Feed {
	for _, f := range snap.Feeds {
		if f.Title == title {
			return f
		}
	}
	t.Fatalf("no feed titled %q", title)
	return nil
}

type rssDoc struct {
	Channel struct {
		Title string `xml:"title"`
		Image struct {
			Href string `xml:"href,attr"`
		} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
		Type  string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd type"`
		Items []struct {
			Title     string `xml:"title"`
			GUID      string `xml:"guid"`
			Enclosure struct {
				URL    string `xml:"url,attr"`
				Length int64  `xml:"length,attr"`
				Type   string `xml:"type,attr"`
			} `xml:"enclosure"`
			Duration string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
			Episode  int    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episode"`
			Chapters struct {
				URL string `xml:"url,attr"`
			} `xml:"https://podcastindex.org/namespace/1.0 chapters"`
		} `xml:"item"`
	} `xml:"channel"`
}

func TestTokenRequired(t *testing.T) {
	s, snap := newServer(t)
	f := snap.Feeds[0]
	for _, target := range []string{"/", "/feeds/" + f.ID + ".xml", "/wrong/feeds/" + f.ID + ".xml"} {
		if resp := get(t, s, target); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, resp.StatusCode)
		}
	}
	if resp := get(t, s, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
	resp := get(t, s, "/"+token)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/"+token+"/" {
		t.Errorf("GET /token = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestIndex(t *testing.T) {
	s, snap := newServer(t)
	resp := get(t, s, "http://nas.local:8080/"+token+"/")
	html := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	f := feedFor(t, snap, "Chaptered Book")
	for _, want := range []string{"Chaptered Book", "Two Parts", "http://nas.local:8080/" + token + "/feeds/" + f.ID + ".xml", "qr.png"} {
		if !strings.Contains(html, want) {
			t.Errorf("index is missing %q", want)
		}
	}
}

func TestFeed(t *testing.T) {
	s, snap := newServer(t)
	f := feedFor(t, snap, "Two Parts")
	resp := get(t, s, "http://nas.local:8080/"+token+"/feeds/"+f.ID+".xml")
	raw := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("feed = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var doc rssDoc
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("feed is not valid XML: %v\n%s", err, raw)
	}
	ch := doc.Channel
	if ch.Title != "Two Parts" || ch.Type != "serial" || len(ch.Items) != 2 {
		t.Fatalf("channel = %q %q with %d items", ch.Title, ch.Type, len(ch.Items))
	}
	if !strings.HasPrefix(ch.Image.Href, "http://nas.local:8080/"+token+"/covers/"+f.ID+"/") {
		t.Errorf("itunes:image = %q", ch.Image.Href)
	}
	newest := ch.Items[0]
	if newest.Title != "02" || newest.Episode != 2 {
		t.Errorf("first item = %q (episode %d), want newest part first", newest.Title, newest.Episode)
	}
	enc := newest.Enclosure
	if !strings.HasPrefix(enc.URL, "http://nas.local:8080/"+token+"/audio/"+f.ID+"/") || !strings.HasSuffix(enc.URL, ".mp3") {
		t.Errorf("enclosure URL = %q", enc.URL)
	}
	if enc.Type != "audio/mpeg" || enc.Length != f.Episodes[1].Size {
		t.Errorf("enclosure = %+v", enc)
	}
	if newest.Duration != "5" {
		t.Errorf("itunes:duration = %q, want 5", newest.Duration)
	}

	// Revalidation: apps polling an unchanged feed get a 304.
	etag := resp.Header.Get("ETag")
	if again := get(t, s, "http://nas.local:8080/"+token+"/feeds/"+f.ID+".xml", "If-None-Match", etag); etag == "" || again.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET with ETag %q = %d, want 304", etag, again.StatusCode)
	}

	// Behind a reverse proxy, links follow the forwarded host and scheme.
	proxied := body(t, get(t, s, "http://10.0.0.5:8080/"+token+"/feeds/"+f.ID+".xml",
		"X-Forwarded-Proto", "https", "X-Forwarded-Host", "books.example.com"))
	if !strings.Contains(proxied, `url="https://books.example.com/`+token+`/audio/`) {
		t.Error("enclosure URLs ignore X-Forwarded-Proto/Host")
	}
}

func TestAudioRange(t *testing.T) {
	s, snap := newServer(t)
	f := feedFor(t, snap, "Two Parts")
	e := f.Episodes[0]
	target := "/" + token + "/audio/" + f.ID + "/" + e.ID + e.Ext

	resp := get(t, s, target, "Range", "bytes=100-199")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range request = %d, want 206", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), "bytes 100-199/"+strconv.FormatInt(e.Size, 10); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if b := body(t, resp); len(b) != 100 {
		t.Errorf("got %d bytes, want 100", len(b))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q", ct)
	}

	full := get(t, s, target)
	if full.StatusCode != http.StatusOK || full.Header.Get("Accept-Ranges") != "bytes" || int64(len(body(t, full))) != e.Size {
		t.Errorf("full download = %d, Accept-Ranges %q", full.StatusCode, full.Header.Get("Accept-Ranges"))
	}

	if resp := get(t, s, "/"+token+"/audio/"+f.ID+"/nope.mp3"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown episode = %d, want 404", resp.StatusCode)
	}
}

func TestChaptersAndCovers(t *testing.T) {
	s, snap := newServer(t)
	f := feedFor(t, snap, "Chaptered Book")
	e := f.Episodes[0]

	raw := body(t, get(t, s, "http://h/"+token+"/feeds/"+f.ID+".xml"))
	var doc rssDoc
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	chURL := doc.Channel.Items[0].Chapters.URL
	if chURL != "http://h/"+token+"/chapters/"+f.ID+"/"+e.ID+".json" {
		t.Fatalf("podcast:chapters url = %q", chURL)
	}
	resp := get(t, s, chURL)
	var chapters struct {
		Version  string `json:"version"`
		Chapters []struct {
			StartTime float64 `json:"startTime"`
			Title     string  `json:"title"`
		} `json:"chapters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chapters); err != nil {
		t.Fatal(err)
	}
	if len(chapters.Chapters) != 3 || chapters.Chapters[1].Title != "The Middle" || chapters.Chapters[1].StartTime < 1.9 {
		t.Errorf("chapters = %+v", chapters)
	}

	// No artwork anywhere: a generated placeholder PNG.
	cover := get(t, s, doc.Channel.Image.Href)
	if b := body(t, cover); cover.StatusCode != http.StatusOK || cover.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(b, "\x89PNG") {
		t.Errorf("placeholder cover = %d %s", cover.StatusCode, cover.Header.Get("Content-Type"))
	}
}

func TestOPMLAndRescan(t *testing.T) {
	s, snap := newServer(t)
	opml := body(t, get(t, s, "http://h/"+token+"/library.opml"))
	for _, f := range snap.Feeds {
		if !strings.Contains(opml, `xmlUrl="http://h/`+token+`/feeds/`+f.ID+`.xml"`) {
			t.Errorf("OPML is missing %q", f.Title)
		}
	}

	r := httptest.NewRequest(http.MethodPost, "http://h/"+token+"/rescan", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "http://h/"+token+"/" {
		t.Errorf("rescan = %d → %q", w.Code, w.Header().Get("Location"))
	}
}
