package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jeffchimney/respool/internal/auth"
)

// With sign-in on, /login and friends live at the root, outside the token,
// so the page can be found before signing in. Redirects use relative URLs
// so ReSpool also works behind a proxy that serves it from a subpath.

//go:embed login.html
var loginHTML string

var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

type loginView struct {
	Setup    bool
	Error    string
	Username string
	Version  string
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.checkSession(w, r) {
		seeOther(w, s.opts.Token+"/")
		return
	}
	seeOther(w, "login")
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.checkSession(w, r) {
		seeOther(w, s.opts.Token+"/")
		return
	}
	s.renderLogin(w, http.StatusOK, loginView{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	store := s.opts.Auth
	if store.NeedsSetup() {
		seeOther(w, "login")
		return
	}
	client := clientIP(r)
	if wait := s.limiter.Wait(client); wait > 0 {
		s.renderLogin(w, http.StatusTooManyRequests, loginView{Error: tooMany(wait)})
		return
	}
	form := readForm(w, r)
	username := form.Get("username")
	if err := store.Login(username, form.Get("password")); err != nil {
		s.limiter.Fail(client)
		s.opts.Logger.Warn("failed sign-in", "client", client)
		s.renderLogin(w, http.StatusUnauthorized, loginView{Error: message(err), Username: username})
		return
	}
	s.limiter.Reset(client)
	s.startSession(w, r)
	s.opts.Logger.Info("signed in", "client", client)
	seeOther(w, s.opts.Token+"/")
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	store := s.opts.Auth
	if !store.NeedsSetup() {
		seeOther(w, "login")
		return
	}
	client := clientIP(r)
	if wait := s.limiter.Wait(client); wait > 0 {
		s.renderLogin(w, http.StatusTooManyRequests, loginView{Error: tooMany(wait)})
		return
	}
	form := readForm(w, r)
	username := form.Get("username")
	err := store.Setup(form.Get("code"), username, form.Get("password"), form.Get("confirm"))
	if err != nil {
		var bad auth.Error
		if !errors.As(err, &bad) {
			s.opts.Logger.Error("creating account", "err", err)
		}
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrWrongSetupCode) {
			s.limiter.Fail(client)
			s.opts.Logger.Warn("wrong setup code", "client", client)
			status = http.StatusUnauthorized
		}
		s.renderLogin(w, status, loginView{Error: message(err), Username: username})
		return
	}
	s.limiter.Reset(client)
	s.startSession(w, r)
	s.opts.Logger.Info("account created", "username", store.Username(), "client", client)
	seeOther(w, s.opts.Token+"/")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
	seeOther(w, "login")
}

type passwordChange struct {
	Current  string `json:"current"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Error string `json:"error,omitempty"`
	}
	if r.Header.Get(uploadHeader) != "ReSpool" {
		writeJSON(w, http.StatusForbidden, response{Error: "Changes have to come from ReSpool's own page."})
		return
	}
	client := clientIP(r)
	if wait := s.limiter.Wait(client); wait > 0 {
		writeJSON(w, http.StatusTooManyRequests, response{Error: tooMany(wait)})
		return
	}
	var c passwordChange
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, response{Error: "That change couldn't be read."})
		return
	}
	if err := s.opts.Auth.ChangePassword(c.Current, c.Password, c.Confirm); err != nil {
		var bad auth.Error
		if !errors.As(err, &bad) {
			s.opts.Logger.Error("changing password", "err", err)
			writeJSON(w, http.StatusInternalServerError, response{Error: "ReSpool couldn't save the new password. Check that its data folder is writable."})
			return
		}
		if errors.Is(err, auth.ErrWrongPassword) {
			s.limiter.Fail(client)
		}
		writeJSON(w, http.StatusBadRequest, response{Error: bad.Error()})
		return
	}
	s.limiter.Reset(client)
	s.startSession(w, r) // the change signed out every browser, this one included
	s.opts.Logger.Info("password changed", "client", client)
	writeJSON(w, http.StatusOK, response{})
}

// checkSession reports whether r comes from a signed-in browser, renewing
// sessions past their halfway point so regular visitors stay signed in.
func (s *Server) checkSession(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(auth.SessionCookie)
	if err != nil {
		return false
	}
	expires, ok := s.opts.Auth.Session(c.Value)
	if ok && time.Until(expires) < auth.SessionLength/2 {
		s.startSession(w, r)
	}
	return ok
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	value, expires := s.opts.Auth.NewSession()
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: value, Path: "/", Expires: expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
}

// signInFirst answers a request that needs a session. Links and forms, like
// Rescan, go to the sign-in page; the page's scripted requests, which carry
// ReSpool's header, get an error the page reacts to.
func (s *Server) signInFirst(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uploadHeader) == "" {
		// r.URL.Path is below the token, so climb out of it to reach /login.
		depth := 1 + strings.Count(strings.TrimPrefix(r.URL.Path, "/"), "/")
		seeOther(w, strings.Repeat("../", depth)+"login")
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "You've been signed out. Reload the page to sign in again."})
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, v loginView) {
	v.Setup = s.opts.Auth.NeedsSetup()
	v.Version = s.opts.Version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, v); err != nil {
		s.opts.Logger.Error("rendering sign-in page", "err", err)
	}
}

func readForm(w http.ResponseWriter, r *http.Request) url.Values {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	r.ParseForm()
	return r.PostForm
}

// seeOther redirects with a relative Location, which browsers resolve
// against the URL they asked for.
func seeOther(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

func message(err error) string {
	var bad auth.Error
	if errors.As(err, &bad) {
		return bad.Error()
	}
	return "Something went wrong. Check ReSpool's logs."
}

func tooMany(wait time.Duration) string {
	minutes := int(math.Ceil(wait.Minutes()))
	if minutes <= 1 {
		return "Too many wrong attempts. Try again in a minute."
	}
	return fmt.Sprintf("Too many wrong attempts. Try again in %d minutes.", minutes)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || firstValue(r.Header.Get("X-Forwarded-Proto")) == "https"
}
