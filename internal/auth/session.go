package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const SessionCookieName = "PVNSession"

var ErrInvalidSession = errors.New("invalid or expired PVN session")

type Session struct {
	ID        string
	CSRFToken string
	Identity  Identity
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	sessions map[string]Session
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &SessionStore{
		ttl:      ttl,
		now:      time.Now,
		sessions: make(map[string]Session),
	}
}

func (s *SessionStore) Create(identity Identity) (Session, error) {
	id, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	session := Session{
		ID: id, CSRFToken: csrf, Identity: identity,
		ExpiresAt: now.Add(s.ttl),
	}
	s.sessions[id] = session
	return session, nil
}

func (s *SessionStore) Get(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	session, ok := s.sessions[id]
	if !ok || !now.Before(session.ExpiresAt) {
		delete(s.sessions, id)
		return Session{}, ErrInvalidSession
	}
	return session, nil
}

func (s *SessionStore) ValidateCSRF(id, token string) (Session, error) {
	session, err := s.Get(id)
	if err != nil {
		return Session{}, err
	}
	if subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) != 1 {
		return Session{}, ErrInvalidSession
	}
	return session, nil
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *SessionStore) pruneLocked(now time.Time) {
	for id, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
