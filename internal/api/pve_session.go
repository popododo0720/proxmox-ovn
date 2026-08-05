package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/auth"
)

const PVNCSRFHeader = "X-PVN-CSRF-Token"

var ErrInvalidCSRF = errors.New("invalid PVN CSRF token")

type PVESessionProviderOptions struct {
	BaseURL     string
	CAFile      string
	ClusterName string
	SessionTTL  time.Duration
	HTTPClient  *http.Client
}

// PVESessionProvider exchanges a verified PVE browser ticket for a short-lived
// PVN session. Unsafe PVN requests require the PVN-specific CSRF token.
type PVESessionProvider struct {
	baseURL     string
	clusterName string
	client      *http.Client
	verifier    *auth.PVEVerifier
	sessions    *auth.SessionStore
}

func NewPVESessionProvider(options PVESessionProviderOptions) (*PVESessionProvider, error) {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid PVE API URL %q", options.BaseURL)
	}
	client := options.HTTPClient
	if client == nil {
		rootCAs, poolErr := x509.SystemCertPool()
		if poolErr != nil || rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		if options.CAFile != "" {
			certificate, readErr := os.ReadFile(options.CAFile)
			if readErr != nil {
				return nil, fmt.Errorf("read PVE CA file: %w", readErr)
			}
			if !rootCAs.AppendCertsFromPEM(certificate) {
				return nil, errors.New("PVE CA file contains no certificates")
			}
		}
		client = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:           http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs},
			},
		}
	}
	verifier, err := auth.NewPVEVerifier(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &PVESessionProvider{
		baseURL:     baseURL,
		clusterName: options.ClusterName,
		client:      client,
		verifier:    verifier,
		sessions:    auth.NewSessionStore(options.SessionTTL),
	}, nil
}

// IssueSession validates PVEAuthCookie, creates a PVNSession cookie, and
// returns the CSRF token to the embedded UI.
func (p *PVESessionProvider) IssueSession(ctx context.Context, writer http.ResponseWriter, incoming *http.Request) (Session, error) {
	ticket, err := auth.TicketFromRequest(incoming)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	identity, err := p.verifier.Verify(ctx, ticket)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return Session{}, ErrUnauthenticated
		}
		return Session{}, err
	}
	stored, err := p.sessions.Create(identity)
	if err != nil {
		return Session{}, err
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    stored.ID,
		Path:     "/api/v1",
		Expires:  stored.ExpiresAt,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	cluster := p.clusterName
	if cluster == "" {
		cluster = p.lookupClusterName(ctx, ticket)
	}
	return sessionResponse(stored, cluster), nil
}

func (p *PVESessionProvider) Session(_ context.Context, incoming *http.Request) (Session, error) {
	cookie, err := incoming.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, ErrUnauthenticated
	}
	stored, err := p.sessions.Get(cookie.Value)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	return sessionResponse(stored, p.clusterName), nil
}

func (p *PVESessionProvider) Authorize(ctx context.Context, incoming *http.Request, unsafe bool) (Session, error) {
	cookie, err := incoming.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, ErrUnauthenticated
	}
	var stored auth.Session
	if unsafe {
		stored, err = p.sessions.ValidateCSRF(cookie.Value, incoming.Header.Get(PVNCSRFHeader))
		if err != nil {
			if _, getErr := p.sessions.Get(cookie.Value); getErr == nil {
				return Session{}, ErrInvalidCSRF
			}
			return Session{}, ErrUnauthenticated
		}
	} else {
		stored, err = p.sessions.Get(cookie.Value)
		if err != nil {
			return Session{}, ErrUnauthenticated
		}
	}
	return sessionResponse(stored, p.clusterName), nil
}

func sessionResponse(stored auth.Session, cluster string) Session {
	permissions := make(map[string]any, len(stored.Identity.Permissions))
	for path, privileges := range stored.Identity.Permissions {
		permissions[path] = privileges
	}
	return Session{User: stored.Identity.User, CSRFToken: stored.CSRFToken, Permissions: permissions, Cluster: cluster}
}

func (p *PVESessionProvider) lookupClusterName(ctx context.Context, ticket string) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api2/json/cluster/status", nil)
	if err != nil {
		return ""
	}
	request.AddCookie(&http.Cookie{Name: auth.PVECookieName, Value: ticket})
	response, err := p.client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	var envelope struct {
		Data []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&envelope) != nil {
		return ""
	}
	for _, entry := range envelope.Data {
		if entry.Type == "cluster" {
			return entry.Name
		}
	}
	return ""
}
