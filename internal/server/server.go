// Package server serves the library over HTTP: an index page, one RSS feed
// per book, and the audio, cover and chapter files those feeds point at.
package server

import (
	"bytes"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jeffchimney/respool/internal/auth"
	"github.com/jeffchimney/respool/internal/feed"
	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/media"
	"github.com/jeffchimney/respool/internal/uploads"
	qrcode "github.com/skip2/go-qrcode"
)

// Options configure a Server.
type Options struct {
	Library *library.Library
	// BaseURL is the public URL feeds should link to, e.g.
	// "https://respool.example.ts.net". Empty means "work it out from each
	// request", which suits LAN use.
	BaseURL string
	// Token, when set, must be the first path segment of every request:
	// http://host:8080/<token>/feeds/…
	Token    string
	Language string
	Version  string
	Logger   *slog.Logger
	// Uploads, when set, lets people add audio from the index page.
	Uploads *uploads.Store
	// UploadLimit caps the size of one upload request, in bytes.
	UploadLimit int64
	// Auth, when set, puts the page and every change behind a sign-in.
	// Feeds stay reachable with the token, since podcast apps can't sign in.
	Auth *auth.Store
}

// Server is an http.Handler for a library.
type Server struct {
	opts         Options
	mux          *http.ServeMux
	authMux      *http.ServeMux // sign-in pages, outside the token
	limiter      *auth.Limiter
	placeholders sync.Map // feed ID → generated cover PNG
}

// New returns a Server for opts.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.UploadLimit <= 0 {
		opts.UploadLimit = DefaultUploadLimit
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /feeds/{file}", s.handleFeed)
	s.mux.HandleFunc("GET /audio/{feed}/{file}", s.handleAudio)
	s.mux.HandleFunc("GET /covers/{feed}/{file}", s.handleCover)
	s.mux.HandleFunc("GET /chapters/{feed}/{file}", s.handleChapters)
	s.mux.HandleFunc("GET /library.opml", s.handleOPML)
	s.mux.HandleFunc("GET /qr.png", s.handleQR)
	s.mux.HandleFunc("GET /icon.svg", s.handleIcon)
	s.mux.HandleFunc("POST /rescan", s.handleRescan)
	s.mux.HandleFunc("POST /upload", s.handleUpload)
	s.mux.HandleFunc("GET /books/{feed}", s.handleBook)
	s.mux.HandleFunc("PATCH /books/{feed}", s.handleEditBook)
	s.mux.HandleFunc("DELETE /books/{feed}", s.handleDeleteBook)
	s.mux.HandleFunc("DELETE /books/{feed}/files/{file}", s.handleDeleteFile)
	if opts.Auth != nil {
		if s.opts.Token == "" {
			// Feeds need a secret of their own once the page has a password.
			s.opts.Token = opts.Auth.FeedToken()
		}
		s.limiter = auth.NewLimiter(10, 15*time.Minute)
		s.authMux = http.NewServeMux()
		s.authMux.HandleFunc("GET /{$}", s.handleRoot)
		s.authMux.HandleFunc("GET /login", s.handleLoginPage)
		s.authMux.HandleFunc("POST /login", s.handleLogin)
		s.authMux.HandleFunc("POST /setup", s.handleSetup)
		s.authMux.HandleFunc("POST /logout", s.handleLogout)
		s.authMux.HandleFunc("GET /icon.svg", s.handleIcon)
		s.mux.HandleFunc("POST /account/password", s.handleChangePassword)
	}
	return s
}

// DefaultUploadLimit is the largest upload accepted when Options doesn't
// say: enough for a long audiobook as a single m4b.
const DefaultUploadLimit = 8 << 30

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.route(rec, r)
	s.opts.Logger.Debug("request", "method", r.Method, "path", s.redact(r.URL.Path),
		"status", rec.status, "range", r.Header.Get("Range"), "took", time.Since(start).Round(time.Microsecond))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
		return
	}
	if s.authMux != nil {
		if h, pattern := s.authMux.Handler(r); pattern != "" {
			h.ServeHTTP(w, r)
			return
		}
	}
	if s.opts.Token != "" {
		seg, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if subtle.ConstantTimeCompare([]byte(seg), []byte(s.opts.Token)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "ReSpool is running. Open it with the URL that includes your access token.\n")
			return
		}
		if rest == "" && !strings.HasSuffix(r.URL.Path, "/") {
			// "/<token>" → "/<token>/" so relative links on the index work.
			http.Redirect(w, r, path.Base(r.URL.Path)+"/", http.StatusMovedPermanently)
			return
		}
		r2 := *r
		u := *r.URL
		u.Path, u.RawPath = "/"+rest, ""
		r2.URL = &u
		r = &r2
	}
	if s.opts.Auth != nil && !feedPath(r.URL.Path) && !s.checkSession(w, r) {
		s.signInFirst(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// feedPath reports whether p (without the token) is something podcast apps
// fetch, which stays open to anyone with the token when sign-in is on.
func feedPath(p string) bool {
	for _, prefix := range []string{"/feeds/", "/audio/", "/covers/", "/chapters/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return p == "/icon.svg"
}

func (s *Server) redact(p string) string {
	if s.opts.Token == "" {
		return p
	}
	return strings.Replace(p, "/"+s.opts.Token, "/{token}", 1)
}

func (s *Server) snapshot() *library.Snapshot { return s.opts.Library.Snapshot() }

// links returns URL builders for the server as the client reached it, so a
// feed pasted as http://nas.local:8080/… points its audio at the same host.
func (s *Server) links(r *http.Request) feed.Links {
	base := s.opts.BaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if p := firstValue(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
			scheme = p
		}
		host := r.Host
		if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
		base = scheme + "://" + host
	}
	if s.opts.Token != "" {
		base += "/" + s.opts.Token
	}
	return feed.Links{Base: base}
}

func firstValue(h string) string {
	v, _, _ := strings.Cut(h, ",")
	return strings.TrimSpace(v)
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	f := s.snapshot().Feed(strings.TrimSuffix(r.PathValue("file"), ".xml"))
	if f == nil {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	opts := feed.Options{Language: s.opts.Language, Generator: "ReSpool " + s.opts.Version}
	if err := feed.WriteRSS(&buf, f, s.links(r), opts); err != nil {
		s.opts.Logger.Error("rendering feed", "feed", f.ID, "err", err)
		http.Error(w, "could not render feed", http.StatusInternalServerError)
		return
	}
	s.opts.Logger.Info("feed requested", "feed", f.Title, "client", r.UserAgent())
	serveBytes(w, r, buf.Bytes(), "application/rss+xml; charset=utf-8", "no-cache")
}

func (s *Server) episode(r *http.Request) (*library.Feed, *library.Episode) {
	f := s.snapshot().Feed(r.PathValue("feed"))
	if f == nil {
		return nil, nil
	}
	name := r.PathValue("file")
	return f, f.Episode(strings.TrimSuffix(name, path.Ext(name)))
}

func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	_, e := s.episode(r)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	fh, err := os.Open(e.Path)
	if err != nil {
		if os.IsNotExist(err) {
			s.opts.Library.Rescan() // the library changed under us
			http.NotFound(w, r)
			return
		}
		s.opts.Logger.Error("opening audio", "file", e.RelPath, "err", err)
		http.Error(w, "could not open file", http.StatusInternalServerError)
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		http.Error(w, "could not open file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", e.MIMEType)
	w.Header().Set("X-Content-Type-Options", "nosniff") // uploaded files are only ever audio
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, st.ModTime().UnixNano(), st.Size()))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	// ServeContent handles Range and conditional requests, which players
	// need for streaming and seeking.
	http.ServeContent(w, r, "", st.ModTime(), fh)
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	f := s.snapshot().Feed(r.PathValue("feed"))
	if f == nil {
		http.NotFound(w, r)
		return
	}
	const cache = "public, max-age=604800" // cover URLs change with the artwork
	switch c := f.Cover; {
	case c.Path != "" && c.Embedded:
		data, mime, err := media.Picture(c.Path)
		if err == nil {
			serveBytes(w, r, data, mime, cache)
			return
		}
		s.opts.Logger.Warn("reading embedded cover", "feed", f.ID, "err", err)
	case c.Path != "":
		fh, err := os.Open(c.Path)
		if err == nil {
			defer fh.Close()
			w.Header().Set("Content-Type", c.MIMEType)
			w.Header().Set("Cache-Control", cache)
			http.ServeContent(w, r, "", c.ModTime, fh)
			return
		}
		s.opts.Logger.Warn("opening cover", "feed", f.ID, "err", err)
	}
	png, ok := s.placeholders.Load(f.ID)
	if !ok {
		png, _ = s.placeholders.LoadOrStore(f.ID, placeholderCover(f.ID))
	}
	serveBytes(w, r, png.([]byte), "image/png", cache)
}

func (s *Server) handleChapters(w http.ResponseWriter, r *http.Request) {
	_, e := s.episode(r)
	if e == nil || len(e.Chapters) == 0 {
		http.NotFound(w, r)
		return
	}
	b, err := feed.ChaptersJSON(e)
	if err != nil {
		http.Error(w, "could not render chapters", http.StatusInternalServerError)
		return
	}
	serveBytes(w, r, b, "application/json+chapters", "no-cache")
}

func (s *Server) handleOPML(w http.ResponseWriter, r *http.Request) {
	var feeds []*library.Feed
	if snap := s.snapshot(); snap != nil {
		feeds = snap.Feeds
	}
	var buf bytes.Buffer
	if err := feed.WriteOPML(&buf, feeds, s.links(r), "ReSpool library"); err != nil {
		http.Error(w, "could not render OPML", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="respool.opml"`)
	serveBytes(w, r, buf.Bytes(), "text/x-opml; charset=utf-8", "no-cache")
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(s.links(r).Index(), qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "could not render QR code", http.StatusInternalServerError)
		return
	}
	serveBytes(w, r, png, "image/png", "no-cache")
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	s.opts.Library.Rescan()
	http.Redirect(w, r, s.links(r).Index(), http.StatusSeeOther)
}

// serveBytes serves b with an ETag so clients can revalidate cheaply.
func serveBytes(w http.ResponseWriter, r *http.Request, b []byte, contentType, cacheControl string) {
	sum := sha1.Sum(b)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ReadFrom keeps the underlying writer's sendfile fast path for audio.
func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(r.ResponseWriter, src)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
