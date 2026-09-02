// Package auth provides password hashing (PBKDF2-SHA256) and cookie sessions.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

const iterations = 600_000

// Hash derives a salted hash for a password.
func Hash(password string) (hash, salt string, err error) {
	s := make([]byte, 16)
	if _, err := rand.Read(s); err != nil {
		return "", "", err
	}
	k, err := pbkdf2.Key(sha256.New, password, s, iterations, 32)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(s), nil
}

// Verify checks a password against hash/salt in constant time.
func Verify(password, hash, salt string) bool {
	s, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return false
	}
	h, err := base64.StdEncoding.DecodeString(hash)
	if err != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, password, s, iterations, 32)
	if err != nil {
		return false
	}
	return hmac.Equal(k, h)
}

// Session is a logged-in browser.
type Session struct {
	Token   string
	UserID  string
	Name    string
	Role    string
	Expires time.Time
	Created time.Time
}

// Sessions is an in-memory session table.
type Sessions struct {
	mu   sync.Mutex
	m    map[string]Session
	TTL  time.Duration
	Nowf func() time.Time
}

// NewSessions creates the table.
func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{m: map[string]Session{}, TTL: ttl, Nowf: time.Now}
}

// Create issues a session token.
func (s *Sessions) Create(userID, name, role string) (Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Session{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	now := s.Nowf()
	sess := Session{Token: tok, UserID: userID, Name: name, Role: role, Expires: now.Add(s.TTL), Created: now}
	s.mu.Lock()
	s.m[tok] = sess
	if len(s.m) > 10000 {
		for k, v := range s.m {
			if now.After(v.Expires) {
				delete(s.m, k)
			}
		}
	}
	s.mu.Unlock()
	return sess, nil
}

// Get returns a live session.
func (s *Sessions) Get(tok string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[tok]
	if !ok || s.Nowf().After(sess.Expires) {
		delete(s.m, tok)
		return Session{}, false
	}
	return sess, true
}

// Delete ends a session.
func (s *Sessions) Delete(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// DeleteUser ends every session of a user.
func (s *Sessions) DeleteUser(userID string) {
	s.mu.Lock()
	for k, v := range s.m {
		if v.UserID == userID {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}
