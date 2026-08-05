package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotManaged  = errors.New("interface is not managed by PVN")
	ErrAmbiguous   = errors.New("interface maps to multiple PVN ports")
	ErrNotBindable = errors.New("PVN port is not bindable")
)

type InterfaceRef struct {
	Node          string
	VMID          int
	NICIndex      int
	InterfaceName string
}

type Resolution struct {
	PortID           string
	LSPName          string
	MACAddress       string
	Generation       string
	RequestedChassis string
	Status           string
}

// ManagerClient resolves the identity encoded in a local PVE TAP name to the
// one logical switch port PVN expects on this chassis.
type ManagerClient interface {
	ResolveInterface(context.Context, InterfaceRef) (Resolution, error)
}

type HTTPManagerClientConfig struct {
	BaseURL       string
	HTTPClient    *http.Client
	CAFile        string
	TLSServerName string
}

type HTTPManagerClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewHTTPManagerClient(config HTTPManagerClientConfig) (*HTTPManagerClient, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" {
		return nil, fmt.Errorf("invalid PVN manager URL %q", config.BaseURL)
	}
	if parsed.Scheme != "unix" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported PVN manager URL scheme %q", parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("PVN manager URL must not contain a query or fragment")
	}

	var httpClient *http.Client
	if parsed.Scheme == "unix" {
		if parsed.Host != "" || !filepath.IsAbs(parsed.Path) {
			return nil, errors.New("PVN manager Unix URL must contain an absolute socket path and no host")
		}
		socketPath := filepath.Clean(parsed.Path)
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
			DisableCompression: true,
		}
		httpClient = cloneHTTPClient(config.HTTPClient)
		httpClient.Transport = transport
		baseURL = "http://pvn-manager.local"
	} else {
		if parsed.Host == "" {
			return nil, errors.New("PVN manager HTTPS URL must include a host")
		}
		httpClient, err = newTLSHTTPClient(config.HTTPClient, config.CAFile, config.TLSServerName)
		if err != nil {
			return nil, err
		}
	}
	return &HTTPManagerClient{baseURL: baseURL, httpClient: httpClient}, nil
}

func cloneHTTPClient(source *http.Client) *http.Client {
	if source == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	copy := *source
	return &copy
}

func newTLSHTTPClient(source *http.Client, caFile, serverName string) (*http.Client, error) {
	client := cloneHTTPClient(source)
	var transport *http.Transport
	switch existing := client.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = existing.Clone()
	default:
		if caFile != "" || serverName != "" {
			return nil, errors.New("manager CA cannot be combined with a non-standard HTTP transport")
		}
		return client, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
		if tlsConfig.InsecureSkipVerify {
			return nil, errors.New("PVN manager TLS verification cannot be disabled")
		}
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
		if serverName != "" {
			tlsConfig.ServerName = serverName
		}
	}
	if caFile != "" {
		certificate, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read PVN manager CA %q: %w", caFile, err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(certificate) {
			return nil, fmt.Errorf("PVN manager CA %q contains no certificates", caFile)
		}
		tlsConfig.RootCAs = roots
	}
	transport.TLSClientConfig = tlsConfig
	client.Transport = transport
	return client, nil
}

type managerResolution struct {
	PortID           string          `json:"port_id"`
	LSPName          string          `json:"lsp_name"`
	MACAddress       string          `json:"mac_address"`
	Generation       json.RawMessage `json:"generation"`
	RequestedChassis string          `json:"requested_chassis"`
	Status           string          `json:"status"`
}

func (client *HTTPManagerClient) ResolveInterface(ctx context.Context, reference InterfaceRef) (Resolution, error) {
	if reference.Node == "" || reference.VMID <= 0 || reference.NICIndex < 0 {
		return Resolution{}, fmt.Errorf("invalid interface reference: %#v", reference)
	}
	query := url.Values{
		"node": []string{reference.Node},
		"vmid": []string{strconv.Itoa(reference.VMID)},
		"nic":  []string{fmt.Sprintf("net%d", reference.NICIndex)},
	}
	endpoint := client.baseURL + "/api/v1/runtime/ports/resolve?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Resolution{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve interface with PVN manager: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Resolution{}, fmt.Errorf("read PVN manager response: %w", err)
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		return Resolution{}, ErrNotManaged
	case http.StatusConflict:
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error.Code == "port_not_bindable" {
			return Resolution{}, ErrNotBindable
		}
		return Resolution{}, ErrAmbiguous
	case http.StatusOK:
		// Continue below.
	default:
		return Resolution{}, fmt.Errorf("PVN manager returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}

	var decoded managerResolution
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Resolution{}, fmt.Errorf("decode PVN manager resolution: %w", err)
	}
	generation, err := decodeGeneration(decoded.Generation)
	if err != nil {
		return Resolution{}, err
	}
	result := Resolution{
		PortID:           decoded.PortID,
		LSPName:          decoded.LSPName,
		MACAddress:       decoded.MACAddress,
		Generation:       generation,
		RequestedChassis: decoded.RequestedChassis,
		Status:           decoded.Status,
	}
	if result.Status != "binding" {
		return Resolution{}, fmt.Errorf("%w: status is %q", ErrNotBindable, result.Status)
	}
	if result.PortID == "" || result.LSPName == "" || result.MACAddress == "" || result.Generation == "" || result.RequestedChassis == "" {
		return Resolution{}, errors.New("PVN manager returned an incomplete port resolution")
	}
	return result, nil
}

func decodeGeneration(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("PVN manager response has no generation")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil || value == "" {
			return "", errors.New("PVN manager response has an invalid generation")
		}
		return value, nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return "", errors.New("PVN manager response has an invalid generation")
	}
	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return "", errors.New("PVN manager generation must be an integer or string")
	}
	return number.String(), nil
}
