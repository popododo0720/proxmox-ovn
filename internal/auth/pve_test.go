package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPVEVerifier(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(PVECookieName)
		if err != nil || cookie.Value != "PVE:user@pve:123::signature" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"/pool/dev": map[string]int{"SDN.Audit": 1, "SDN.Allocate": 0},
			},
		})
	}))
	defer server.Close()
	client := server.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server only
	verifier, err := NewPVEVerifier(server.URL, client)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(context.Background(), "PVE:user@pve:123::signature")
	if err != nil {
		t.Fatal(err)
	}
	if identity.User != "user@pve" || !identity.Allows("/pool/dev", "SDN.Audit") {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	if identity.Allows("/pool/dev", "SDN.Allocate") {
		t.Fatal("zero-valued privilege must be denied")
	}
}

func TestUserFromTicketRejectsUntrustedShape(t *testing.T) {
	if _, err := userFromTicket("not-a-pve-ticket"); err == nil {
		t.Fatal("expected malformed ticket rejection")
	}
}
