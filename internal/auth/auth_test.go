package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost
	os.Exit(m.Run())
}

func setUp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Setup(s.SetupCode(), "Owner", "correct horse", "correct horse"); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestOpenKeepsTheFeedToken(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.FeedToken()) != 32 || !s.NeedsSetup() {
		t.Fatalf("new store: token %q, needs setup %v", s.FeedToken(), s.NeedsSetup())
	}
	code := s.SetupCode()
	if len(code) != 9 || code[4] != '-' || strings.ContainsAny(code, "01IO") {
		t.Errorf("setup code = %q", code)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.FeedToken() != s.FeedToken() {
		t.Error("the feed token changed when the store was reopened")
	}
	if again.SetupCode() == code {
		t.Error("the setup code should change every time ReSpool starts")
	}
	st, err := os.Stat(filepath.Join(dir, stateFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
}

func TestOpenFailsWhenNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if _, err := Open(dir); err == nil {
		t.Fatal("Open succeeded in a read-only folder")
	}
}

func TestSetup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code := s.SetupCode()
	for _, tt := range []struct {
		code, user, pass, confirm string
		want                      string
	}{
		{"WRONG-CODE", "me", "long enough", "long enough", string(ErrWrongSetupCode)},
		{code, "  ", "long enough", "long enough", "Choose a username."},
		{code, "me", "short", "short", "at least 8"},
		{code, "me", strings.Repeat("x", 73), strings.Repeat("x", 73), "too long"},
		{code, "me", "long enough", "different", "don't match"},
	} {
		err := s.Setup(tt.code, tt.user, tt.pass, tt.confirm)
		var e Error
		if !errors.As(err, &e) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Setup(%q, %q, …) = %v, want %q", tt.code, tt.user, err, tt.want)
		}
	}
	if !s.NeedsSetup() {
		t.Fatal("a failed setup created an account")
	}
	// The code works lowercase and without its dash.
	if err := s.Setup(strings.ToLower(strings.Replace(code, "-", "", 1)), " Owner ", "long enough", "long enough"); err != nil {
		t.Fatal(err)
	}
	if s.NeedsSetup() || s.Username() != "Owner" || s.SetupCode() != "" {
		t.Errorf("after setup: needs setup %v, user %q, code %q", s.NeedsSetup(), s.Username(), s.SetupCode())
	}
	if err := s.Setup(code, "other", "long enough", "long enough"); err != ErrAlreadySetUp {
		t.Errorf("second setup = %v, want ErrAlreadySetUp", err)
	}
}

func TestLogin(t *testing.T) {
	s, dir := setUp(t)
	for _, tt := range []struct {
		user, pass string
		ok         bool
	}{
		{"Owner", "correct horse", true},
		{"owner", "correct horse", true}, // usernames ignore case
		{"Owner", "wrong horse", false},
		{"someone", "correct horse", false},
		{"", "", false},
	} {
		if err := s.Login(tt.user, tt.pass); (err == nil) != tt.ok {
			t.Errorf("Login(%q, %q) = %v", tt.user, tt.pass, err)
		}
	}
	// The account survives a restart, and the password isn't stored.
	again, _ := Open(dir)
	if err := again.Login("Owner", "correct horse"); err != nil || !strings.Contains(readFile(t, dir), "$2a$") {
		t.Errorf("after reopening: %v", err)
	}
	if strings.Contains(readFile(t, dir), "correct horse") {
		t.Error("auth.json contains the password")
	}
}

func TestSessions(t *testing.T) {
	s, _ := setUp(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	value, expires := s.NewSession()
	if got, ok := s.Session(value); !ok || !got.Equal(expires) {
		t.Fatalf("Session(new) = %v %v", got, ok)
	}
	for name, v := range map[string]string{
		"empty":    "",
		"garbage":  "not-a-session",
		"tampered": strings.Replace(value, ".1.", ".2.", 1),
		"bad sig":  value[:len(value)-2] + "xx",
	} {
		if _, ok := s.Session(v); ok {
			t.Errorf("%s session accepted", name)
		}
	}
	now = now.Add(SessionLength + time.Second)
	if _, ok := s.Session(value); ok {
		t.Error("expired session accepted")
	}
}

func TestChangePasswordSignsOutEverywhere(t *testing.T) {
	s, _ := setUp(t)
	old, _ := s.NewSession()
	if err := s.ChangePassword("wrong", "new password", "new password"); err != ErrWrongPassword {
		t.Errorf("wrong current password = %v", err)
	}
	if err := s.ChangePassword("correct horse", "new", "new"); err == nil {
		t.Error("accepted a short new password")
	}
	if err := s.ChangePassword("correct horse", "new password", "new password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Session(old); ok {
		t.Error("a session from before the change still works")
	}
	if s.Login("Owner", "correct horse") == nil || s.Login("Owner", "new password") != nil {
		t.Error("login doesn't use the new password")
	}
	if v, _ := s.NewSession(); func() bool { _, ok := s.Session(v); return !ok }() {
		t.Error("new sessions don't work after the change")
	}
}

func TestReset(t *testing.T) {
	s, dir := setUp(t)
	session, _ := s.NewSession()
	token := s.FeedToken()
	if err := Reset(dir); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NeedsSetup() || again.SetupCode() == "" {
		t.Error("reset didn't remove the account")
	}
	if again.FeedToken() != token {
		t.Error("reset changed the feed token, which would break subscriptions")
	}
	again.Setup(again.SetupCode(), "Owner", "correct horse", "correct horse")
	if _, ok := again.Session(session); ok {
		t.Error("a session from before the reset still works")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if w := l.Wait("a"); w != 0 {
			t.Fatalf("attempt %d has to wait %v", i+1, w)
		}
		l.Fail("a")
		now = now.Add(time.Second)
	}
	if w := l.Wait("a"); w <= 0 || w > time.Minute {
		t.Errorf("after 3 failures, wait = %v", w)
	}
	if l.Wait("b") != 0 {
		t.Error("one client's failures held up another")
	}
	now = now.Add(time.Minute)
	if w := l.Wait("a"); w != 0 {
		t.Errorf("after the window, wait = %v", w)
	}
	l.Fail("a")
	l.Reset("a")
	if l.Wait("a") != 0 || len(l.failures) != 0 {
		t.Error("Reset didn't forget the failures")
	}
}

func readFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
