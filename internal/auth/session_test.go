package auth

import (
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	store := NewSessionStore(time.Minute)
	now := time.Unix(1000, 0)
	store.now = func() time.Time { return now }
	session, err := store.Create(Identity{User: "root@pam"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateCSRF(session.ID, session.CSRFToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateCSRF(session.ID, "wrong"); err == nil {
		t.Fatal("wrong CSRF token accepted")
	}
	now = now.Add(2 * time.Minute)
	if _, err := store.Get(session.ID); err == nil {
		t.Fatal("expired session accepted")
	}
}
