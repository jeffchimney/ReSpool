package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/uploads"
)

// Uploaded books can be edited and deleted from the page. Books in the media
// folder can't: ReSpool never writes there.

type bookDetails struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Author      string        `json:"author"`
	Description string        `json:"description"`
	Cover       string        `json:"cover"`
	Files       []fileDetails `json:"files"`
}

type fileDetails struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Size     string `json:"size"`
	Duration string `json:"duration,omitempty"`
}

type bookResponse struct {
	Title       string `json:"title,omitempty"`
	BookDeleted bool   `json:"bookDeleted,omitempty"`
	Error       string `json:"error,omitempty"`
}

// uploadedBook finds the uploaded book a request is about, or writes an
// error and returns nil. Requests that change anything must carry ReSpool's
// header, for the same reason uploads must.
func (s *Server) uploadedBook(w http.ResponseWriter, r *http.Request) *library.Feed {
	if s.opts.Uploads == nil {
		http.NotFound(w, r)
		return nil
	}
	if r.Method != http.MethodGet && r.Header.Get(uploadHeader) != "ReSpool" {
		writeJSON(w, http.StatusForbidden, bookResponse{Error: "Changes have to come from ReSpool's own page."})
		return nil
	}
	f := s.snapshot().Feed(r.PathValue("feed"))
	if f == nil || !s.opts.Uploads.Holds(f.Dir) {
		writeJSON(w, http.StatusNotFound, bookResponse{Error: "That book isn't one you uploaded, so it can't be changed here."})
		return nil
	}
	return f
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	f := s.uploadedBook(w, r)
	if f == nil {
		return
	}
	o := library.ReadOverrides(f.Dir)
	d := bookDetails{
		ID:          f.ID,
		Title:       f.Title,
		Author:      f.Author,
		Description: firstNonEmpty(o.Description, f.Description),
		Cover:       s.links(r).Cover(f),
		Files:       []fileDetails{},
	}
	for _, e := range f.Episodes {
		d.Files = append(d.Files, fileDetails{
			ID:       e.ID,
			Name:     filepath.Base(e.Path),
			Title:    e.Title,
			Size:     humanBytes(e.Size),
			Duration: humanDuration(e.Duration),
		})
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleEditBook(w http.ResponseWriter, r *http.Request) {
	f := s.uploadedBook(w, r)
	if f == nil {
		return
	}
	var o library.Overrides
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&o); err != nil {
		writeJSON(w, http.StatusBadRequest, bookResponse{Error: "That edit couldn't be read."})
		return
	}
	if err := s.opts.Uploads.Edit(f.Dir, o); err != nil {
		s.bookError(w, "editing book", err)
		return
	}
	out := bookResponse{}
	if g := feedInDir(s.rescanNow(), f.Dir); g != nil {
		out.Title = g.Title
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	f := s.uploadedBook(w, r)
	if f == nil {
		return
	}
	if err := s.opts.Uploads.Delete(f.Dir); err != nil {
		s.bookError(w, "deleting book", err)
		return
	}
	s.rescanNow()
	writeJSON(w, http.StatusOK, bookResponse{Title: f.Title, BookDeleted: true})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	f := s.uploadedBook(w, r)
	if f == nil {
		return
	}
	e := f.Episode(r.PathValue("file"))
	if e == nil {
		writeJSON(w, http.StatusNotFound, bookResponse{Error: "That file is already gone."})
		return
	}
	gone, err := s.opts.Uploads.DeleteFile(f.Dir, e.Path)
	if err != nil {
		s.bookError(w, "deleting file", err)
		return
	}
	s.rescanNow()
	writeJSON(w, http.StatusOK, bookResponse{Title: f.Title, BookDeleted: gone})
}

func (s *Server) bookError(w http.ResponseWriter, doing string, err error) {
	var bad *uploads.UserError
	if errors.As(err, &bad) {
		writeJSON(w, http.StatusBadRequest, bookResponse{Error: bad.Msg})
		return
	}
	s.opts.Logger.Error(doing, "err", err)
	writeJSON(w, http.StatusInternalServerError, bookResponse{Error: "ReSpool couldn't change the uploads folder. Check that it's writable."})
}

// rescanNow scans immediately so the page shows the change when it reloads.
func (s *Server) rescanNow() *library.Snapshot {
	snap, err := s.opts.Library.Scan()
	if err != nil {
		s.opts.Logger.Error("scanning after a change", "err", err)
		return s.snapshot()
	}
	return snap
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
