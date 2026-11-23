package server

import (
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML string

// markSVG pairs the iOS Spool cassette with ReSpool's broadcast signal.
//
//go:embed mark.svg
var markSVG []byte

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	serveBytes(w, r, markSVG, "image/svg+xml", "public, max-age=86400")
}

type bookView struct {
	ID, Title, Author, FeedURL, CoverURL string
	Episodes                             int
	Duration                             string
	Search                               string
	Uploaded                             bool // can be edited and deleted from the page
}

type indexView struct {
	Version  string
	Ready    bool
	Scanning bool
	Books    []bookView
	Episodes int
	Duration string
	OPMLURL  string
	QRURL    string
	Loopback bool
	TokenSet bool
	Scanned  string

	Uploads     bool
	UploadLimit string
	UploadBooks []uploadBook // books that more files can be added to

	Login    bool // sign-in is on
	Username string
}

type uploadBook struct{ ID, Title string }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	links := s.links(r)
	snap := s.snapshot()
	v := indexView{
		Version:  s.opts.Version,
		Ready:    snap != nil,
		Scanning: s.opts.Library.Scanning(),
		OPMLURL:  links.OPML(),
		QRURL:    links.Base + "/qr.png",
		Loopback: isLoopback(links.Base),
		TokenSet: s.opts.Token != "",
		Uploads:  s.opts.Uploads != nil,
	}
	if v.Uploads {
		v.UploadLimit = humanBytes(s.opts.UploadLimit)
	}
	if s.opts.Auth != nil {
		v.Login, v.Username = true, s.opts.Auth.Username()
	}
	if snap != nil {
		var total time.Duration
		for _, f := range snap.Feeds {
			uploaded := v.Uploads && s.opts.Uploads.Holds(f.Dir)
			v.Books = append(v.Books, bookView{
				ID:       f.ID,
				Title:    f.Title,
				Author:   f.Author,
				FeedURL:  links.Feed(f),
				CoverURL: links.Cover(f),
				Episodes: len(f.Episodes),
				Duration: humanDuration(f.Duration),
				Search:   strings.ToLower(f.Title + " " + f.Author + " " + f.Path),
				Uploaded: uploaded,
			})
			v.Episodes += len(f.Episodes)
			total += f.Duration
			if uploaded {
				v.UploadBooks = append(v.UploadBooks, uploadBook{ID: f.ID, Title: f.Title})
			}
		}
		v.Duration = humanDuration(total)
		v.Scanned = snap.ScannedAt.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := indexTmpl.Execute(w, v); err != nil {
		s.opts.Logger.Error("rendering index", "err", err)
	}
}

func isLoopback(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	m := int(d.Round(time.Minute).Minutes())
	switch {
	case m == 0:
		return "<1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	default:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
}
