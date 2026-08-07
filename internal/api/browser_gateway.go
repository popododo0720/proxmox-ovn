package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const (
	PVEAuthIDHeader      = "X-PVN-PVE-Authid"
	PVEPermissionsHeader = "X-PVN-PVE-Permissions"
	maxGatewayHeaderSize = 256 << 10
)

var pveAuthIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}@[A-Za-z0-9][A-Za-z0-9._-]{0,63}(?:![A-Za-z0-9][A-Za-z0-9._-]{0,63})?$`)

// BrowserHandler accepts only requests forwarded by the local PVE API
// extension. The surrounding Unix listener is responsible for authenticating
// the peer as the pveproxy www-data user before a request can reach it.
func (s *Server) BrowserHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !browserRouteAllowed(request.Method, request.URL.Path) {
			writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
			return
		}
		if request.URL.Query().Has("project_id") {
			writeError(writer, http.StatusBadRequest, "projectless_api", "project_id is not part of the PVN API", nil)
			return
		}
		if request.Body != nil && request.ContentLength != 0 {
			body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
			if err != nil || len(body) > maxRequestBody {
				writeError(writer, http.StatusBadRequest, "invalid_request", "request body is invalid or too large", nil)
				return
			}
			_ = request.Body.Close()
			if containsProjectID(body) {
				writeError(writer, http.StatusBadRequest, "projectless_api", "project_id is not part of the PVN API", nil)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.ContentLength = int64(len(body))
		}

		session, err := gatewaySession(request, s.clusterName)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthenticated", "the trusted Proxmox identity is missing or invalid", nil)
			return
		}
		request.Header.Del(PVEAuthIDHeader)
		request.Header.Del(PVEPermissionsHeader)
		request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, session))
		s.ServeHTTP(writer, request)
	})
}

func gatewaySession(request *http.Request, cluster string) (Session, error) {
	authIDs := request.Header.Values(PVEAuthIDHeader)
	encodedPermissions := request.Header.Values(PVEPermissionsHeader)
	if len(authIDs) != 1 || len(encodedPermissions) != 1 || !pveAuthIDPattern.MatchString(authIDs[0]) {
		return Session{}, ErrUnauthenticated
	}
	if len(encodedPermissions[0]) == 0 || len(encodedPermissions[0]) > maxGatewayHeaderSize {
		return Session{}, ErrUnauthenticated
	}
	raw, err := base64.RawURLEncoding.DecodeString(encodedPermissions[0])
	if err != nil || len(raw) == 0 || len(raw) > maxGatewayHeaderSize {
		return Session{}, ErrUnauthenticated
	}
	var permissions map[string]map[string]int
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&permissions); err != nil || permissions == nil {
		return Session{}, ErrUnauthenticated
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Session{}, ErrUnauthenticated
	}
	normalized := make(map[string]any, len(permissions))
	for path, privileges := range permissions {
		if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00") || len(path) > 1024 || privileges == nil {
			return Session{}, ErrUnauthenticated
		}
		values := make(map[string]bool, len(privileges))
		for privilege, value := range privileges {
			if privilege == "" || len(privilege) > 128 || strings.ContainsAny(privilege, "\r\n\x00") || (value != 0 && value != 1) {
				return Session{}, ErrUnauthenticated
			}
			values[privilege] = value == 1
		}
		normalized[path] = values
	}
	return Session{User: authIDs[0], Permissions: normalized, Cluster: cluster}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("unexpected trailing JSON")
}

func containsProjectID(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, found := object["project_id"]
	return found
}

func browserRouteAllowed(method, path string) bool {
	if method == http.MethodGet && (path == "/api/v1/health" || path == "/api/v1/runtime/ports/resolve") {
		return true
	}
	if method == http.MethodPost && path == "/api/v1/ports/provision" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/")
	if len(parts) < 1 || len(parts) > 3 || parts[0] == "" || !browserCollection(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		if parts[0] == "operations" {
			return method == http.MethodGet
		}
		return method == http.MethodGet || method == http.MethodPost
	}
	if parts[1] == "" || !safeGatewayID(parts[1]) {
		return false
	}
	if len(parts) == 2 {
		if parts[0] == "operations" {
			return method == http.MethodGet
		}
		return method == http.MethodGet || method == http.MethodPut || method == http.MethodDelete
	}
	if parts[0] != "ports" {
		return false
	}
	switch parts[2] {
	case "attach", "detach":
		return method == http.MethodPost
	case "deprovision":
		return method == http.MethodDelete
	default:
		return false
	}
}

func browserCollection(value string) bool {
	switch value {
	case "networks", "subnets", "ports", "ip-allocations", "routers", "router-interfaces",
		"floating-ips", "provider-networks", "provider-segments", "security-groups",
		"security-group-rules", "nodes", "operations":
		return true
	default:
		return false
	}
}

func safeGatewayID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}
