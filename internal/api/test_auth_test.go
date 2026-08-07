package api

import (
	"context"
	"net/http"
)

// These helpers model the authentication boundary in unit tests. Production
// requests receive their Session only from BrowserHandler after the local
// pveproxy gateway has authenticated the caller and enforced PVE CSRF.
type SessionProvider interface {
	Session(context.Context, *http.Request) (Session, error)
}

type SessionProviderFunc func(context.Context, *http.Request) (Session, error)

func (function SessionProviderFunc) Session(ctx context.Context, request *http.Request) (Session, error) {
	return function(ctx, request)
}

type testAPIHandler struct {
	*Server
	provider SessionProvider
}

func newTestAPIHandler(server *Server, provider SessionProvider) *testAPIHandler {
	return &testAPIHandler{Server: server, provider: provider}
}

func (handler *testAPIHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	session := Session{
		User: "root@pam",
		Permissions: map[string]any{"/": map[string]bool{
			"SDN.Allocate":      true,
			"SDN.Audit":         true,
			"SDN.Use":           true,
			"Sys.Modify":        true,
			"VM.Config.Network": true,
		}},
	}
	if handler.provider != nil {
		var err error
		session, err = handler.provider.Session(request.Context(), request)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthenticated", "a test session is required", nil)
			return
		}
	}
	request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, session))
	handler.Server.ServeHTTP(writer, request)
}
