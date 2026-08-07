package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotManaged      = errors.New("interface is not managed by PVN")
	ErrAmbiguous       = errors.New("interface maps to multiple PVN ports")
	ErrNotBindable     = errors.New("PVN port is not bindable")
	ErrStaleGeneration = errors.New("PVN port report generation is stale")
)

const (
	PortStatusUnbound   = "unbound"
	PortStatusBinding   = "binding"
	PortStatusBound     = "bound"
	PortStatusDetaching = "detaching"
	PortStatusError     = "error"
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

type PortReport struct {
	PortID     string
	Generation string
	Status     string
}

type NodeHeartbeat struct {
	Name          string
	ChassisID     string
	Roles         []string
	RolesExplicit bool
	OnlineNodes   []string
	Quorate       *bool
}

// ManagerClient resolves the identity encoded in a local PVE TAP name to the
// one logical switch port PVN expects on this chassis.
type ManagerClient interface {
	ResolveInterface(context.Context, InterfaceRef) (Resolution, error)
	ReportPort(context.Context, PortReport) error
}

type HTTPManagerClientConfig struct {
	BaseURL    string
	HTTPClient *http.Client
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
	if parsed.Scheme != "unix" {
		return nil, errors.New("PVN manager URL must use a Unix socket")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("PVN manager URL must not contain a query or fragment")
	}

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
	httpClient := cloneHTTPClient(config.HTTPClient)
	httpClient.Transport = transport
	return &HTTPManagerClient{baseURL: "http://pvn-manager.local", httpClient: httpClient}, nil
}

func cloneHTTPClient(source *http.Client) *http.Client {
	if source == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	copy := *source
	return &copy
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
	switch result.Status {
	case PortStatusBinding, PortStatusBound:
		if result.PortID == "" || result.LSPName == "" || result.MACAddress == "" || result.Generation == "" || result.RequestedChassis == "" {
			return Resolution{}, errors.New("PVN manager returned an incomplete bindable port resolution")
		}
	case PortStatusDetaching, PortStatusUnbound, PortStatusError:
		if result.PortID == "" || result.Generation == "" {
			return Resolution{}, errors.New("PVN manager returned an incomplete port cleanup resolution")
		}
	default:
		return Resolution{}, fmt.Errorf("PVN manager returned unknown port status %q", result.Status)
	}
	return result, nil
}

func (client *HTTPManagerClient) ReportPort(ctx context.Context, report PortReport) error {
	if report.PortID == "" || (report.Status != PortStatusBound && report.Status != PortStatusUnbound) {
		return errors.New("port ID and a bound or unbound report status are required")
	}
	generation, err := strconv.ParseInt(report.Generation, 10, 64)
	if err != nil || generation < 1 {
		return errors.New("port report generation must be a positive integer")
	}
	payload, err := json.Marshal(map[string]any{"generation": generation, "status": report.Status})
	if err != nil {
		return err
	}
	endpoint := client.baseURL + "/api/v1/runtime/ports/" + url.PathEscape(report.PortID) + "/report"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("report port state to PVN manager: %w", err)
	}
	defer response.Body.Close()
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read PVN manager report response: %w", err)
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode == http.StatusConflict {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(responsePayload, &failure) == nil && failure.Error.Code == "stale_generation" {
			return ErrStaleGeneration
		}
	}
	return fmt.Errorf("PVN manager returned HTTP %d for port report: %s", response.StatusCode, strings.TrimSpace(string(responsePayload)))
}

func (client *HTTPManagerClient) HeartbeatNode(ctx context.Context, heartbeat NodeHeartbeat) error {
	if strings.TrimSpace(heartbeat.Name) == "" || strings.TrimSpace(heartbeat.ChassisID) == "" {
		return errors.New("node name and chassis ID are required")
	}
	payload := map[string]any{"name": heartbeat.Name, "chassis_id": heartbeat.ChassisID}
	if heartbeat.RolesExplicit {
		if len(heartbeat.Roles) == 0 {
			return errors.New("an explicit node role list must not be empty")
		}
		payload["roles"] = heartbeat.Roles
	}
	if (heartbeat.OnlineNodes == nil) != (heartbeat.Quorate == nil) {
		return errors.New("online node membership and quorum state must be supplied together")
	}
	if heartbeat.OnlineNodes != nil {
		if len(heartbeat.OnlineNodes) == 0 {
			return errors.New("online node membership must not be empty")
		}
		payload["online_nodes"] = heartbeat.OnlineNodes
		payload["quorate"] = *heartbeat.Quorate
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/api/v1/runtime/nodes/heartbeat", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send node heartbeat to PVN manager: %w", err)
	}
	defer response.Body.Close()
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read PVN manager heartbeat response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("PVN manager returned HTTP %d for node heartbeat: %s", response.StatusCode, strings.TrimSpace(string(responsePayload)))
	}
	return nil
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
