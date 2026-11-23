package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffchimney/respool/internal/auth"
	"github.com/jeffchimney/respool/internal/library"
)

const password = "correct horse"

// newLoginServer serves a one-book library with sign-in on and no
// RESPOOL_TOKEN, so feeds use the token the store generates.
func newLoginServer(t *testing.T) (*Server, *auth.Store, *library.Feed) {
	t.Helper()
	media := t.TempDir()
	data, err := os.ReadFile("../media/testdata/tagged.mp3")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(media, "Book"), 0o755)
	os.WriteFile(filepath.Join(media, "Book", "01.mp3"), data, 0o644)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	lib := library.New(logger, library.Root{Dir: media})
	snap, err := lib.Scan()
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Library: lib, Logger: logger, Version: "test", Auth: store}), store, snap.Feeds[0]
}

// do sends a request with an optional session cookie and form or JSON body.
func do(t *testing.T, s *Server, method, target, cookie string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://nas.local:8080"+target, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:5000"
	switch {
	case strings.HasPrefix(body, "{"):
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Requested-With", "ReSpool")
	case body != "":
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func session(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	return nil
}

func form(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, location string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != location {
		t.Errorf("got %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), location)
	}
}

func TestSignIn(t *testing.T) {
	s, store, book := newLoginServer(t)
	token := store.FeedToken()
	page := "/" + token + "/"

	// Before there's an account: everything about the page leads to setup,
	// without giving the token away.
	wantRedirect(t, do(t, s, "GET", "/", "", ""), "login")
	wantRedirect(t, do(t, s, "GET", page, "", ""), "../login")
	wantRedirect(t, do(t, s, "GET", page+"library.opml", "", ""), "../login")
	wantRedirect(t, do(t, s, "POST", page+"rescan", "", ""), "../login") // a plain form
	if rec := do(t, s, "GET", "/login", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Create your account") ||
		strings.Contains(rec.Body.String(), token) {
		t.Fatalf("setup page = %d, or it shows the token", rec.Code)
	}
	if rec := do(t, s, "PATCH", page+"books/"+book.ID, "", `{"title":"x"}`); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "signed out") {
		t.Errorf("API without a session = %d %s, want 401", rec.Code, rec.Body)
	}

	// Feeds, audio and covers work for podcast apps, with the token.
	for _, p := range []string{"feeds/" + book.ID + ".xml", "audio/" + book.ID + "/" + book.Episodes[0].ID + ".mp3", "icon.svg"} {
		if rec := do(t, s, "GET", page+p, "", ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a session = %d, want 200", p, rec.Code)
		}
	}
	if rec := do(t, s, "GET", "/feeds/"+book.ID+".xml", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("feed without the token = %d, want 404", rec.Code)
	}
	if rec := do(t, s, "GET", "/icon.svg", "", ""); rec.Code != http.StatusOK {
		t.Errorf("the sign-in page's icon = %d", rec.Code)
	}

	// Creating the account needs the setup code from the logs.
	rec := do(t, s, "POST", "/setup", "", form("code", "WRON-GCOD", "username", "owner", "password", password, "confirm", password))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "setup code") || session(rec) != nil {
		t.Fatalf("setup with a wrong code = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/setup", "", form("code", store.SetupCode(), "username", "owner", "password", password, "confirm", password))
	wantRedirect(t, rec, token+"/")
	c := session(rec)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("session cookie = %+v", c)
	}

	// Signed in, the page works and has the account button.
	rec = do(t, s, "GET", page, c.Value, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="account"`) || !strings.Contains(rec.Body.String(), "owner") {
		t.Fatalf("page with a session = %d", rec.Code)
	}
	wantRedirect(t, do(t, s, "GET", "/", c.Value, ""), token+"/")
	wantRedirect(t, do(t, s, "GET", "/login", c.Value, ""), token+"/")
	if rec := do(t, s, "GET", page+"library.opml", c.Value, ""); rec.Code != 200 {
		t.Errorf("OPML with a session = %d", rec.Code)
	}

	// Signing out clears the cookie; signing in again needs the password.
	rec = do(t, s, "POST", "/logout", c.Value, "")
	wantRedirect(t, rec, "login")
	if out := session(rec); out == nil || out.MaxAge >= 0 {
		t.Errorf("sign-out cookie = %+v", out)
	}
	if rec := do(t, s, "GET", "/login", "", ""); !strings.Contains(rec.Body.String(), "Sign in") {
		t.Error("after setup, /login doesn't show the sign-in form")
	}
	rec = do(t, s, "POST", "/login", "", form("username", "owner", "password", "wrong password"))
	if rec.Code != http.StatusUnauthorized || session(rec) != nil || !strings.Contains(rec.Body.String(), `value="owner"`) {
		t.Errorf("wrong password = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/login", "", form("username", "Owner", "password", password))
	wantRedirect(t, rec, token+"/")
	if session(rec) == nil {
		t.Error("signing in set no cookie")
	}
}

func TestSignInRateLimit(t *testing.T) {
	s, store, _ := newLoginServer(t)
	store.Setup(store.SetupCode(), "owner", password, password)
	s.limiter = auth.NewLimiter(2, time.Minute)
	for i := 0; i < 2; i++ {
		do(t, s, "POST", "/login", "", form("username", "owner", "password", "guess"))
	}
	rec := do(t, s, "POST", "/login", "", form("username", "owner", "password", password))
	if rec.Code != http.StatusTooManyRequests || session(rec) != nil || !strings.Contains(rec.Body.String(), "Try again") {
		t.Errorf("third attempt = %d, want 429 even with the right password", rec.Code)
	}
}

func TestChangePassword(t *testing.T) {
	s, store, _ := newLoginServer(t)
	store.Setup(store.SetupCode(), "owner", password, password)
	page := "/" + store.FeedToken() + "/"
	old, _ := store.NewSession()

	rec := do(t, s, "POST", page+"account/password", old, `{"current":"wrong","password":"new password","confirm":"new password"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current password") {
		t.Errorf("wrong current password = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, s, "POST", page+"account/password", old, `{"current":"`+password+`","password":"new password","confirm":"new password"}`)
	c := session(rec)
	if rec.Code != http.StatusOK || c == nil {
		t.Fatalf("changing the password = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "GET", page, c.Value, ""); rec.Code != 200 {
		t.Errorf("the browser that changed it was signed out: %d", rec.Code)
	}
	wantRedirect(t, do(t, s, "GET", page, old, ""), "../login")
}

func TestSignInOff(t *testing.T) {
	s, _ := newServer(t)
	if rec := do(t, s, "GET", "/login", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("/login with sign-in off = %d, want 404", rec.Code)
	}
	if html := body(t, get(t, s, "/"+token+"/")); strings.Contains(html, `id="account"`) {
		t.Error("index shows the account button with sign-in off")
	}
}
