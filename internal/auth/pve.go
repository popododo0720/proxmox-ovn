package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const PVECookieName = "PVEAuthCookie"

var ErrUnauthenticated = errors.New("PVE session is not authenticated")

type Identity struct {
	User        string                     `json:"user"`
	Permissions map[string]map[string]bool `json:"permissions"`
}

type PVEVerifier struct {
	BaseURL *url.URL
	Client  *http.Client
}

func NewPVEVerifier(rawURL string, client *http.Client) (*PVEVerifier, error) {
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("invalid PVE API URL %q", rawURL)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &PVEVerifier{BaseURL: base, Client: client}, nil
}

// Verify asks the local PVE API to validate the ticket and return the current
// user's effective permission map. PVN never accepts a username supplied by
// the browser as proof of identity.
func (v *PVEVerifier) Verify(ctx context.Context, ticket string) (Identity, error) {
	if ticket == "" {
		return Identity{}, ErrUnauthenticated
	}
	endpoint := *v.BaseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api2/json/access/permissions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Identity{}, err
	}
	req.AddCookie(&http.Cookie{Name: PVECookieName, Value: ticket, Path: "/", Secure: true})
	resp, err := v.Client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("validate PVE ticket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Identity{}, ErrUnauthenticated
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("PVE permissions API returned %s", resp.Status)
	}
	var envelope struct {
		Data map[string]map[string]int `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return Identity{}, fmt.Errorf("decode PVE permissions: %w", err)
	}
	permissions := make(map[string]map[string]bool, len(envelope.Data))
	for path, raw := range envelope.Data {
		permissions[path] = make(map[string]bool, len(raw))
		for privilege, value := range raw {
			permissions[path][privilege] = value != 0
		}
	}
	user, err := userFromTicket(ticket)
	if err != nil {
		return Identity{}, err
	}
	return Identity{User: user, Permissions: permissions}, nil
}

func TicketFromRequest(r *http.Request) (string, error) {
	cookie, err := r.Cookie(PVECookieName)
	if err != nil || cookie.Value == "" {
		return "", ErrUnauthenticated
	}
	return cookie.Value, nil
}

func userFromTicket(ticket string) (string, error) {
	decoded, err := url.QueryUnescape(ticket)
	if err != nil {
		return "", ErrUnauthenticated
	}
	parts := strings.Split(decoded, ":")
	if len(parts) < 4 || parts[0] != "PVE" || parts[1] == "" || !strings.Contains(parts[1], "@") {
		return "", ErrUnauthenticated
	}
	return parts[1], nil
}

func (i Identity) Allows(path string, privileges ...string) bool {
	available := i.Permissions[path]
	for _, privilege := range privileges {
		if !available[privilege] {
			return false
		}
	}
	return true
}
