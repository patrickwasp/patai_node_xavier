package auth

import (
	"encoding/base64"
	"sync"
	"time"
)

// Sessions live only in memory. They end with the daemon, so there's nothing to revoke on disk and
// nothing to write to the eMMC on each request, and time.Since runs on the monotonic clock, so a unit
// that doesn't know the date can't stretch or cut one short.
type Session struct {
	UserID  int64
	Name    string
	Role    string
	started time.Time
	seen    time.Time
}

type Sessions struct {
	mu   sync.Mutex
	byID map[string]*Session
	Idle time.Duration
	Max  time.Duration
}

func NewSessions() *Sessions {
	return &Sessions{byID: map[string]*Session{}, Idle: 2 * time.Hour, Max: 12 * time.Hour}
}

func (s *Sessions) Create(userID int64, name, role string) string {
	id := base64.RawURLEncoding.EncodeToString(random(32))
	t := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[HashSecret(id)] = &Session{UserID: userID, Name: name, Role: role, started: t, seen: t}
	return id
}

// Get finds a live session. Touch says a person did something: a page left open polls for status all
// day, and that mustn't keep a session alive past its idle limit.
func (s *Sessions) Get(id string, touch bool) *Session {
	key := HashSecret(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byID[key]
	if sess == nil {
		return nil
	}
	if time.Since(sess.seen) > s.Idle || time.Since(sess.started) > s.Max {
		delete(s.byID, key)
		return nil
	}
	if touch {
		sess.seen = time.Now()
	}
	copied := *sess
	return &copied
}

func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, HashSecret(id))
}

// DeleteUser ends a user's sessions, keeping one if it's given: a password change keeps the session
// that made it and ends the others.
func (s *Sessions) DeleteUser(userID int64, keep string) {
	keepKey := ""
	if keep != "" {
		keepKey = HashSecret(keep)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sess := range s.byID {
		if sess.UserID == userID && key != keepKey {
			delete(s.byID, key)
		}
	}
}

// Throttle slows guessing: a few free attempts, then a delay that doubles up to 15 minutes.
type Throttle struct {
	mu   sync.Mutex
	seen map[string]*attempts
}

type attempts struct {
	fails int
	until time.Time
}

const (
	freeAttempts = 5
	baseDelay    = 5 * time.Second
	maxDelay     = 15 * time.Minute
	maxTracked   = 4096
)

func NewThrottle() *Throttle { return &Throttle{seen: map[string]*attempts{}} }

func (t *Throttle) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.seen[key]
	return a == nil || time.Now().After(a.until)
}

func (t *Throttle) Fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.seen) >= maxTracked {
		t.seen = map[string]*attempts{}
	}
	a := t.seen[key]
	if a == nil {
		a = &attempts{}
		t.seen[key] = a
	}
	a.fails++
	if a.fails >= freeAttempts {
		delay := baseDelay << (a.fails - freeAttempts)
		if delay > maxDelay || delay <= 0 {
			delay = maxDelay
		}
		a.until = time.Now().Add(delay)
	}
}

func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, key)
}
