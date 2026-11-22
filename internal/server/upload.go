package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/uploads"
)

// uploadHeader must accompany uploads. A browser can't add a custom header
// to a cross-site request without a CORS preflight, which ReSpool never
// approves, so another website can't upload through a visitor's browser.
const uploadHeader = "X-Requested-With"

type uploadResponse struct {
	Title   string `json:"title,omitempty"`
	FeedURL string `json:"feedUrl,omitempty"`
	Files   int    `json:"files,omitempty"`
	Cover   bool   `json:"cover,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	store := s.opts.Uploads
	if store == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(uploadHeader) != "ReSpool" {
		writeJSON(w, http.StatusForbidden, uploadResponse{Error: "Uploads have to come from ReSpool's own page."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.UploadLimit)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResponse{Error: "That wasn't a file upload."})
		return
	}

	res, err := store.Receive(mr, func(id string) (string, bool) {
		if f := s.snapshot().Feed(id); f != nil {
			return f.Dir, true
		}
		return "", false
	})
	var tooBig *http.MaxBytesError
	var bad *uploads.UserError
	switch {
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, uploadResponse{
			Error: fmt.Sprintf("That upload is over the %s limit.", humanBytes(s.opts.UploadLimit)),
		})
		return
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, uploadResponse{Error: bad.Msg})
		return
	case err != nil:
		s.opts.Logger.Error("storing upload", "err", err)
		writeJSON(w, http.StatusInternalServerError, uploadResponse{
			Error: "ReSpool couldn't save the upload. Check that its uploads folder is writable and has space.",
		})
		return
	}

	// Scan now rather than waiting for the next scheduled scan, so the book
	// is on the page when it reloads.
	out := uploadResponse{Files: len(res.Files), Cover: res.Cover}
	if f := feedInDir(s.rescanNow(), res.Dir); f != nil {
		out.Title, out.FeedURL = f.Title, s.links(r).Feed(f)
	}
	s.opts.Logger.Info("upload added", "book", out.Title, "files", out.Files, "cover", out.Cover)
	writeJSON(w, http.StatusOK, out)
}

func feedInDir(snap *library.Snapshot, dir string) *library.Feed {
	if snap == nil {
		return nil
	}
	for _, f := range snap.Feeds {
		if f.Dir == dir {
			return f
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// humanBytes uses 1024-based units, so the default limit of 8192 MiB reads
// as "8 GB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %cB", int64(v), "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %cB", v, "KMGTPE"[exp])
}
