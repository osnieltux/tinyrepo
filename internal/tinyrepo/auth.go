package tinyrepo

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The admin panel is the one place where -ws accepts input rather than only
// handing files out, so everything it needs to not be a way in lives here:
// password hashing, sessions, CSRF and a brake on guessing.

const (
	// pbkdf2Iterations is what a new hash is generated with. The stored hash
	// carries its own count, so raising this does not invalidate old ones.
	pbkdf2Iterations = 600_000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
	// hashScheme prefixes a stored hash so a future algorithm can be told apart
	// from this one instead of being guessed at.
	hashScheme = "pbkdf2-sha256"

	// sessionCookie is deliberately host-only and prefixed: __Host- means the
	// browser refuses it from a subdomain and without Secure+Path=/, so a
	// neighbouring host on the same site cannot plant one. Only over HTTPS,
	// hence the plain name as the fallback below.
	sessionCookie       = "tinyrepo_session"
	sessionSecureCookie = "__Host-tinyrepo_session"

	// sessionIdle ends a session that stopped being used; sessionMax ends one
	// however active it is, so a stolen cookie is not good forever.
	sessionIdle = 30 * time.Minute
	sessionMax  = 12 * time.Hour

	// loginWindow and loginBurst are the guessing brake: past loginBurst
	// failures from one address, another attempt is refused until the window
	// has passed. Successful logins clear it.
	loginWindow = 15 * time.Minute
	loginBurst  = 5
)

var (
	errNoCredentials = errors.New("no credentials configured")
	errBadHash       = errors.New("unusable password hash")
)

// hashPassword returns the stored form of a password:
// "pbkdf2-sha256$<iterations>$<salt b64>$<key b64>". The salt is per password,
// so two identical passwords do not produce the same hash.
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeHash(password, salt, pbkdf2Iterations)
}

func encodeHash(password string, salt []byte, iterations int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, iterations,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the stored hash. It always
// runs the full derivation and compares in constant time, so neither the answer
// nor how long it took says which half was wrong.
func verifyPassword(stored, password string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false, errBadHash
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return false, errBadHash
	}

	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false, errBadHash
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, errBadHash
	}

	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// randomToken returns a URL-safe random string of n bytes of entropy.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// session is one logged-in browser.
type session struct {
	user string
	// csrf is per session rather than per form: a form is only accepted with
	// the token belonging to the session that is submitting it, which is what
	// stops another site from posting on the user's behalf.
	csrf    string
	created time.Time
	seen    time.Time
}

// sessionStore holds live sessions in memory. Restarting -ws logs everyone out,
// which for a tool that is started and stopped by hand is the right trade
// against keeping a signing key on disk.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session

	// failures counts recent failed logins per client address.
	failures map[string]*loginFailures
}

type loginFailures struct {
	count int
	since time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: map[string]*session{},
		failures: map[string]*loginFailures{},
	}
}

// create starts a session and returns its cookie value.
func (s *sessionStore) create(user string) (string, *session, error) {
	id, err := randomToken(32)
	if err != nil {
		return "", nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return "", nil, err
	}

	now := time.Now()
	sess := &session{user: user, csrf: csrf, created: now, seen: now}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(now)
	s.sessions[id] = sess
	return id, sess, nil
}

// get returns the live session for an id, refreshing its idle deadline. An
// expired session is dropped rather than returned.
func (s *sessionStore) get(id string) (*session, bool) {
	if id == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}

	now := time.Now()
	if now.Sub(sess.seen) > sessionIdle || now.Sub(sess.created) > sessionMax {
		delete(s.sessions, id)
		return nil, false
	}

	sess.seen = now
	return sess, true
}

func (s *sessionStore) destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// evictExpiredLocked drops dead sessions. Called on create, so the map is
// bounded by how many logins are actually live rather than by how many there
// have ever been.
func (s *sessionStore) evictExpiredLocked(now time.Time) {
	for id, sess := range s.sessions {
		if now.Sub(sess.seen) > sessionIdle || now.Sub(sess.created) > sessionMax {
			delete(s.sessions, id)
		}
	}
	for addr, f := range s.failures {
		if now.Sub(f.since) > loginWindow {
			delete(s.failures, addr)
		}
	}
}

// throttled reports whether this client has failed too often too recently.
func (s *sessionStore) throttled(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.failures[addr]
	if !ok {
		return false
	}
	if time.Since(f.since) > loginWindow {
		delete(s.failures, addr)
		return false
	}
	return f.count >= loginBurst
}

func (s *sessionStore) recordFailure(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.failures[addr]
	if !ok || time.Since(f.since) > loginWindow {
		s.failures[addr] = &loginFailures{count: 1, since: time.Now()}
		return
	}
	f.count++
}

func (s *sessionStore) clearFailures(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, addr)
}

// clientAddr is the key the login brake counts against. Deliberately the real
// peer address unless a proxy is configured: X-Forwarded-For is set by whoever
// is talking to us, and counting against a header anyone can spoof would let an
// attacker both dodge the brake and lock somebody else out of it.
func clientAddr(r *http.Request, behindProxy bool) string {
	if behindProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopback reports whether an address literal is a loopback host. Used to
// decide how loudly to warn about publishing the panel.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" {
		return false // an empty host is every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authCredentials are the configured login, checked once at startup so a panel
// that could never be logged into is reported then rather than discovered on
// the login page.
func authCredentials(config *Config) (user, hash string, err error) {
	user = strings.TrimSpace(config.Web.User)
	hash = strings.TrimSpace(config.Web.PasswordHash)

	if user == "" || hash == "" {
		return "", "", errNoCredentials
	}
	// Parse it now: a truncated or mistyped hash otherwise looks exactly like a
	// wrong password, forever.
	if _, err := verifyPassword(hash, ""); err != nil && errors.Is(err, errBadHash) {
		return "", "", errBadHash
	}
	return user, hash, nil
}
