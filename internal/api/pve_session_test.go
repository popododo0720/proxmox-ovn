package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/auth"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
)

func TestPVESessionExchangeAndCSRFProtection(t *testing.T) {
	pve := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cookie, err := request.Cookie(auth.PVECookieName)
		if err != nil || cookie.Value == "" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api2/json/access/permissions":
			_, _ = writer.Write([]byte(`{"data":{"/":{"SDN.Audit":1,"SDN.Allocate":1}}}`))
		case "/api2/json/cluster/status":
			_, _ = writer.Write([]byte(`{"data":[{"type":"cluster","name":"lab"}]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer pve.Close()
	provider, err := NewPVESessionProvider(PVESessionProviderOptions{BaseURL: pve.URL, HTTPClient: pve.Client(), SessionTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, controlstore.NewMemory(), provider)

	ticket := url.QueryEscape("PVE:root@pam:01234567::signature")
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	sessionRequest.AddCookie(&http.Cookie{Name: auth.PVECookieName, Value: ticket})
	sessionRecorder := httptest.NewRecorder()
	server.ServeHTTP(sessionRecorder, sessionRequest)
	if sessionRecorder.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", sessionRecorder.Code, sessionRecorder.Body.String())
	}
	var envelope struct {
		Data Session `json:"data"`
	}
	if err := json.Unmarshal(sessionRecorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.User != "root@pam" || envelope.Data.Cluster != "lab" || envelope.Data.CSRFToken == "" {
		t.Fatalf("session = %#v", envelope.Data)
	}
	result := sessionRecorder.Result()
	var pvnCookie *http.Cookie
	for _, cookie := range result.Cookies() {
		if cookie.Name == auth.SessionCookieName {
			pvnCookie = cookie
		}
	}
	if pvnCookie == nil || !pvnCookie.HttpOnly || !pvnCookie.Secure || pvnCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("PVN cookie = %#v", pvnCookie)
	}

	post := func(csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/networks", strings.NewReader(`{"name":"private"}`))
		req.AddCookie(pvnCookie)
		req.Header.Set("Idempotency-Key", "create-network")
		if csrf != "" {
			req.Header.Set(PVNCSRFHeader, csrf)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	withoutCSRF := post("")
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("without CSRF status=%d body=%s", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	withCSRF := post(envelope.Data.CSRFToken)
	if withCSRF.Code != http.StatusCreated {
		t.Fatalf("with CSRF status=%d body=%s", withCSRF.Code, withCSRF.Body.String())
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/networks", nil)
	getRequest.AddCookie(pvnCookie)
	getRecorder := httptest.NewRecorder()
	server.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("authenticated GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
}

func TestPVESessionUsesConfiguredDeploymentName(t *testing.T) {
	pve := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api2/json/cluster/status" {
			t.Error("configured deployment name unexpectedly fell back to the PVE status API")
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		if request.URL.Path != "/api2/json/access/permissions" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"/":{"SDN.Audit":1}}}`))
	}))
	defer pve.Close()
	provider, err := NewPVESessionProvider(PVESessionProviderOptions{
		BaseURL: pve.URL, HTTPClient: pve.Client(), ClusterName: "human-cluster", SessionTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(&http.Cookie{Name: auth.PVECookieName, Value: url.QueryEscape("PVE:root@pam:01234567::signature")})
	recorder := httptest.NewRecorder()
	session, err := provider.IssueSession(context.Background(), recorder, request)
	if err != nil {
		t.Fatal(err)
	}
	if session.Cluster != "human-cluster" {
		t.Fatalf("issued session deployment name=%q", session.Cluster)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == auth.SessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("PVN session cookie is missing")
	}
	followup := httptest.NewRequest(http.MethodGet, "/api/v1/networks", nil)
	followup.AddCookie(sessionCookie)
	persisted, err := provider.Session(context.Background(), followup)
	if err != nil || persisted.Cluster != "human-cluster" {
		t.Fatalf("persisted session deployment name=%q err=%v", persisted.Cluster, err)
	}
}

func TestPVESessionRejectsBadTicket(t *testing.T) {
	pve := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusUnauthorized) }))
	defer pve.Close()
	provider, err := NewPVESessionProvider(PVESessionProviderOptions{BaseURL: pve.URL, HTTPClient: pve.Client()})
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, controlstore.NewMemory(), provider)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: auth.PVECookieName, Value: url.QueryEscape("PVE:root@pam:bad")})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPVESessionConfiguredCAMustExist(t *testing.T) {
	_, err := NewPVESessionProvider(PVESessionProviderOptions{BaseURL: "https://pve.example.test:8006", CAFile: "/definitely/not/a/pve-ca.pem"})
	if err == nil {
		t.Fatal("missing configured CA was ignored")
	}
}
