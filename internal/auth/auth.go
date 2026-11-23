// Package auth keeps ReSpool's optional owner account: one username and
// password that guard the page and every change to the library. Feeds stay
// on their secret URLs, because podcast apps can't sign in.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// SessionCookie holds a signed-in browser's session.
	SessionCookie = "respool_session"
	// SessionLength is how long a browser stays signed in.
	SessionLength = 30 * 24 * time.Hour

	stateFile   = "auth.json"
	minPassword = 8  // characters
	maxPassword = 72 // bytes: bcrypt ignores anything longer
	maxUsername = 64 // characters
)

// bcryptCost takes roughly a tenth of a second on a desktop and under a
// second on a Raspberry Pi. Tests lower it.
var bcryptCost = 11

// Error is a problem the person signing in can fix. Its text is shown to them.
type Error string

func (e Error) Error() string { return string(e) }

const (
	ErrWrongLogin     Error = "That username and password don't match."
	ErrWrongSetupCode Error = "That setup code isn't right. The current one is in ReSpool's logs."
	ErrWrongPassword  Error = "Your current password isn't right."
	ErrAlreadySetUp   Error = "An account already exists, so sign in instead."
	ErrNoAccount      Error = "There's no account yet."
)

type account struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
	// Epoch is part of every session. Changing it signs out every browser.
	Epoch   int       `json:"epoch"`
	Created time.Time `json:"created"`
}

type state struct {
	// FeedToken keeps feed URLs secret when RESPOOL_TOKEN isn't set.
	FeedToken string `json:"feedToken"`
	// SessionKey signs session cookies.
	SessionKey []byte   `json:"sessionKey"`
	Account    *account `json:"account,omitempty"`
}

// Store is the account, kept in auth.json in ReSpool's data folder.
type Store struct {
	path      string
	now       func() time.Time
	mu        sync.Mutex
	st        state
	setupCode string // while there's no account
}

// Open loads the account in dir, creating the folder and file if needed. It
// fails if dir isn't writable, so a server that should ask for a password
// never starts without one.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, stateFile), now: time.Now}
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if s.st.FeedToken == "" {
		s.st.FeedToken = hex.EncodeToString(randomBytes(16))
	}
	if len(s.st.SessionKey) < 32 {
		s.st.SessionKey = randomBytes(32)
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	if s.st.Account == nil {
		s.setupCode = newSetupCode()
	}
	return s, nil
}

// Reset removes the account in dir and signs out every browser, keeping the
// feed token so subscriptions keep working. A running server picks it up
// when it restarts.
func Reset(dir string) error {
	s, err := Open(dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Account = nil
	s.st.SessionKey = randomBytes(32)
	return s.save()
}

// FeedToken is a secret for feed URLs, made once and kept.
func (s *Store) FeedToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.FeedToken
}

// NeedsSetup reports whether the account still has to be created.
func (s *Store) NeedsSetup() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Account == nil
}

// SetupCode is the code that proves whoever creates the account can read
// the server's logs. It changes every time ReSpool starts.
func (s *Store) SetupCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.setupCode) != 8 {
		return ""
	}
	return s.setupCode[:4] + "-" + s.setupCode[4:]
}

// Username is the account's username, or "" before setup.
func (s *Store) Username() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account == nil {
		return ""
	}
	return s.st.Account.Username
}

// Setup creates the account, given the setup code from the logs.
func (s *Store) Setup(code, username, password, confirm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account != nil {
		return ErrAlreadySetUp
	}
	if !codeMatches(code, s.setupCode) {
		return ErrWrongSetupCode
	}
	username = strings.TrimSpace(username)
	if err := checkUsername(username); err != nil {
		return err
	}
	hash, err := hashPassword(password, confirm)
	if err != nil {
		return err
	}
	s.st.Account = &account{Username: username, PasswordHash: hash, Epoch: 1, Created: s.now().UTC()}
	if err := s.save(); err != nil {
		s.st.Account = nil
		return err
	}
	s.setupCode = ""
	return nil
}

// Login checks a username and password. Usernames ignore case.
func (s *Store) Login(username, password string) error {
	a, ok := s.account()
	if !ok {
		return ErrNoAccount
	}
	// Check the password even when the username is wrong, so the two take
	// the same time.
	wrongPassword := bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(password)) != nil
	if wrongPassword || !strings.EqualFold(strings.TrimSpace(username), a.Username) {
		return ErrWrongLogin
	}
	return nil
}

// ChangePassword replaces the password and signs out every browser. The
// caller starts a new session for the browser that made the change.
func (s *Store) ChangePassword(current, password, confirm string) error {
	a, ok := s.account()
	if !ok {
		return ErrNoAccount
	}
	if bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(current)) != nil {
		return ErrWrongPassword
	}
	hash, err := hashPassword(password, confirm)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account == nil || s.st.Account.PasswordHash != a.PasswordHash {
		return Error("Your password was changed somewhere else. Reload the page and try again.")
	}
	old := s.st.Account
	updated := *old
	updated.PasswordHash = hash
	updated.Epoch++
	s.st.Account = &updated
	if err := s.save(); err != nil {
		s.st.Account = old
		return err
	}
	return nil
}

func (s *Store) account() (account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account == nil {
		return account{}, false
	}
	return *s.st.Account, true
}

// NewSession returns a cookie value that proves this browser signed in, and
// when it expires.
func (s *Store) NewSession() (value string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account == nil {
		return "", time.Time{}
	}
	expires = s.now().Add(SessionLength).Truncate(time.Second)
	payload := strconv.FormatInt(expires.Unix(), 10) + "." + strconv.Itoa(s.st.Account.Epoch)
	return payload + "." + s.sign(payload), expires
}

// Session reports whether value is a current session, and when it expires.
func (s *Store) Session(value string) (expires time.Time, ok bool) {
	exp, rest, _ := strings.Cut(value, ".")
	epoch, sig, _ := strings.Cut(rest, ".")
	payload := exp + "." + epoch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Account == nil || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return time.Time{}, false
	}
	unix, err1 := strconv.ParseInt(exp, 10, 64)
	n, err2 := strconv.Atoi(epoch)
	if err1 != nil || err2 != nil || n != s.st.Account.Epoch {
		return time.Time{}, false
	}
	expires = time.Unix(unix, 0)
	if !s.now().Before(expires) {
		return time.Time{}, false
	}
	return expires, true
}

func (s *Store) sign(payload string) string {
	mac := hmac.New(sha256.New, s.st.SessionKey)
	mac.Write([]byte("respool-session:" + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// save writes the state atomically, readable only by ReSpool's user.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func checkUsername(u string) error {
	switch {
	case u == "":
		return Error("Choose a username.")
	case utf8.RuneCountInString(u) > maxUsername:
		return Error(fmt.Sprintf("Use a username of at most %d characters.", maxUsername))
	}
	return nil
}

func hashPassword(password, confirm string) (string, error) {
	switch {
	case utf8.RuneCountInString(password) < minPassword:
		return "", Error(fmt.Sprintf("Use a password of at least %d characters.", minPassword))
	case len(password) > maxPassword:
		return "", Error(fmt.Sprintf("That password is too long. Use at most %d characters.", maxPassword))
	case password != confirm:
		return "", Error("The passwords don't match.")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(hash), err
}

// Setup codes avoid characters that are easy to misread: 0/O, 1/I.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newSetupCode() string {
	b := randomBytes(8)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)] // 256 is a multiple of 32: no bias
	}
	return string(b)
}

func codeMatches(input, code string) bool {
	if code == "" {
		return false
	}
	input = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(input))
	return subtle.ConstantTimeCompare([]byte(input), []byte(code)) == 1
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never fails: crypto/rand panics rather than return an error
	return b
}
