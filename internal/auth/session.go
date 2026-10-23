package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// Session adalah sesi login di panel.
type Session struct {
	Username  string
	Role      Role
	CreatedAt time.Time
	LastSeen  time.Time
}

// Sessions menyimpan sesi di memori. Yang disimpan hanya hash token, jadi
// isi memori yang bocor pun tidak bisa dipakai untuk masuk. Sesi hilang saat
// sapo-api di-restart (user cukup login ulang).
type Sessions struct {
	Idle time.Duration
	Max  time.Duration

	mu   sync.Mutex
	byID map[[32]byte]*Session
	now  func() time.Time
}

// NewSessions membuat pengelola sesi.
func NewSessions(idle, max time.Duration) *Sessions {
	return &Sessions{Idle: idle, Max: max, byID: map[[32]byte]*Session{}, now: time.Now}
}

func hashToken(tok string) [32]byte { return sha256.Sum256([]byte(tok)) }

// Create membuat sesi baru dan mengembalikan token untuk cookie.
func (s *Sessions) Create(username string, role Role) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	s.byID[hashToken(tok)] = &Session{Username: username, Role: role, CreatedAt: now, LastSeen: now}
	return tok, nil
}

// Get mengembalikan sesi yang masih berlaku dan memperbarui LastSeen.
func (s *Sessions) Get(tok string) (Session, bool) {
	if tok == "" {
		return Session{}, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hashToken(tok)
	sess, ok := s.byID[id]
	if !ok {
		return Session{}, false
	}
	if now.Sub(sess.LastSeen) > s.Idle || now.Sub(sess.CreatedAt) > s.Max {
		delete(s.byID, id)
		return Session{}, false
	}
	sess.LastSeen = now
	return *sess, true
}

// Delete mengakhiri sesi.
func (s *Sessions) Delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, hashToken(tok))
}

func (s *Sessions) gcLocked(now time.Time) {
	for id, sess := range s.byID {
		if now.Sub(sess.LastSeen) > s.Idle || now.Sub(sess.CreatedAt) > s.Max {
			delete(s.byID, id)
		}
	}
}
