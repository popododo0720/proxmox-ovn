package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultTaskPollInterval = time.Second

// Auth applies one of Proxmox's supported API authentication mechanisms.
type Auth interface {
	apply(*http.Request)
}

type TicketAuth struct {
	Ticket    string
	CSRFToken string
}

func (auth TicketAuth) apply(request *http.Request) {
	if auth.Ticket != "" {
		request.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: auth.Ticket})
	}
	if auth.CSRFToken != "" && request.Method != http.MethodGet && request.Method != http.MethodHead {
		request.Header.Set("CSRFPreventionToken", auth.CSRFToken)
	}
}

// APITokenAuth represents the token identifier and secret without combining
// or logging them. TokenID is normally user@realm!token-name.
type APITokenAuth struct {
	TokenID string
	Secret  string
}

func (auth APITokenAuth) apply(request *http.Request) {
	if auth.TokenID != "" || auth.Secret != "" {
		request.Header.Set("Authorization", "PVEAPIToken="+auth.TokenID+"="+auth.Secret)
	}
}

type ClientConfig struct {
	BaseURL          string
	HTTPClient       *http.Client
	Auth             Auth
	TaskPollInterval time.Duration
}

type Client struct {
	baseURL          string
	httpClient       *http.Client
	auth             Auth
	taskPollInterval time.Duration
}

func NewClient(config ClientConfig) (*Client, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid PVE base URL %q", config.BaseURL)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported PVE URL scheme %q", parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("PVE base URL must not contain a query or fragment")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	pollInterval := config.TaskPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultTaskPollInterval
	}
	return &Client{
		baseURL:          baseURL,
		httpClient:       httpClient,
		auth:             config.Auth,
		taskPollInterval: pollInterval,
	}, nil
}

// APIError describes an HTTP error returned by the PVE REST API.
type APIError struct {
	StatusCode int
	Message    string
}

func (err *APIError) Error() string {
	if err.Message == "" {
		return fmt.Sprintf("PVE API returned HTTP %d", err.StatusCode)
	}
	return fmt.Sprintf("PVE API returned HTTP %d: %s", err.StatusCode, err.Message)
}

type apiEnvelope struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors,omitempty"`
}

func (client *Client) request(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if client.auth != nil {
		client.auth.apply(request)
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("PVE API %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read PVE API response: %w", err)
	}
	var envelope apiEnvelope
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &envelope); err != nil {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil, fmt.Errorf("decode PVE API response: %w", err)
			}
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(payload))
		if len(envelope.Errors) > 0 {
			parts := make([]string, 0, len(envelope.Errors))
			for key, value := range envelope.Errors {
				parts = append(parts, key+": "+value)
			}
			message = strings.Join(parts, "; ")
		}
		return nil, &APIError{StatusCode: response.StatusCode, Message: message}
	}
	return envelope.Data, nil
}

// VMConfig contains only the network-specific portion needed by the agent.
// Raw remains available for diagnostics without lossy interface conversion.
type VMConfig struct {
	Digest   string
	Networks map[int]NetProperty
	Raw      map[string]json.RawMessage
}

func (client *Client) GetVMConfig(ctx context.Context, node string, vmid int) (VMConfig, error) {
	if err := validateNodeAndVM(node, vmid); err != nil {
		return VMConfig{}, err
	}
	data, err := client.request(ctx, http.MethodGet, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), nil)
	if err != nil {
		return VMConfig{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return VMConfig{}, fmt.Errorf("decode VM config: %w", err)
	}

	config := VMConfig{Networks: make(map[int]NetProperty), Raw: raw}
	if digest, ok := raw["digest"]; ok {
		if err := json.Unmarshal(digest, &config.Digest); err != nil {
			return VMConfig{}, fmt.Errorf("decode VM config digest: %w", err)
		}
	}
	for key, encoded := range raw {
		index, ok := parseNetKey(key)
		if !ok {
			continue
		}
		var propertyText string
		if err := json.Unmarshal(encoded, &propertyText); err != nil {
			return VMConfig{}, fmt.Errorf("decode %s: %w", key, err)
		}
		property, err := ParseNetProperty(propertyText)
		if err != nil {
			return VMConfig{}, fmt.Errorf("decode %s: %w", key, err)
		}
		config.Networks[index] = property
	}
	return config, nil
}

// VMNetworkClient is the subset used by the hotplug state machine.
type VMNetworkClient interface {
	GetVMConfig(context.Context, string, int) (VMConfig, error)
	SetVMNetwork(context.Context, string, int, int, NetProperty, string) (string, error)
	DeleteVMNetwork(context.Context, string, int, int, string) (string, error)
	WaitUPID(context.Context, string, string) error
}

// SetVMNetwork updates one netN property using PVE's optimistic digest check.
// It returns a UPID when the endpoint schedules an asynchronous task.
func (client *Client) SetVMNetwork(ctx context.Context, node string, vmid, index int, property NetProperty, digest string) (string, error) {
	if err := validateNetworkMutation(node, vmid, index, digest); err != nil {
		return "", err
	}
	form := url.Values{
		fmt.Sprintf("net%d", index): []string{property.String()},
		"digest":                    []string{digest},
	}
	data, err := client.request(ctx, http.MethodPut, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), form)
	if err != nil {
		return "", err
	}
	return decodeOptionalUPID(data)
}

func (client *Client) DeleteVMNetwork(ctx context.Context, node string, vmid, index int, digest string) (string, error) {
	if err := validateNetworkMutation(node, vmid, index, digest); err != nil {
		return "", err
	}
	form := url.Values{
		"delete": []string{fmt.Sprintf("net%d", index)},
		"digest": []string{digest},
	}
	data, err := client.request(ctx, http.MethodPut, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), form)
	if err != nil {
		return "", err
	}
	return decodeOptionalUPID(data)
}

func decodeOptionalUPID(data json.RawMessage) (string, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return "", nil
	}
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return "", fmt.Errorf("decode PVE task UPID: %w", err)
	}
	return upid, nil
}

type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

type UPIDPoller interface {
	GetTaskStatus(context.Context, string, string) (TaskStatus, error)
	WaitUPID(context.Context, string, string) error
}

func (client *Client) GetTaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	if err := validateIdentifier("node", node); err != nil {
		return TaskStatus{}, err
	}
	if upid == "" || strings.ContainsAny(upid, "/\r\n") {
		return TaskStatus{}, errors.New("invalid PVE task UPID")
	}
	data, err := client.request(ctx, http.MethodGet, fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), nil)
	if err != nil {
		return TaskStatus{}, err
	}
	var status TaskStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return TaskStatus{}, fmt.Errorf("decode PVE task status: %w", err)
	}
	return status, nil
}

func (client *Client) WaitUPID(ctx context.Context, node, upid string) error {
	if upid == "" {
		return nil
	}
	for {
		status, err := client.GetTaskStatus(ctx, node, upid)
		if err != nil {
			return err
		}
		if status.Status == "stopped" {
			if status.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("PVE task %s failed: %s", upid, status.ExitStatus)
		}

		timer := time.NewTimer(client.taskPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func validateNodeAndVM(node string, vmid int) error {
	if err := validateIdentifier("node", node); err != nil {
		return err
	}
	if vmid <= 0 {
		return fmt.Errorf("invalid VM ID %d", vmid)
	}
	return nil
}

func validateNetworkMutation(node string, vmid, index int, digest string) error {
	if err := validateNodeAndVM(node, vmid); err != nil {
		return err
	}
	if index < 0 {
		return fmt.Errorf("invalid NIC index %d", index)
	}
	if strings.TrimSpace(digest) == "" || strings.ContainsAny(digest, "\r\n") {
		return errors.New("a valid PVE config digest is required")
	}
	return nil
}

func validateIdentifier(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("invalid %s %q", kind, value)
	}
	return nil
}

func parseNetKey(key string) (int, bool) {
	if !strings.HasPrefix(key, "net") || len(key) == 3 {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(key, "net"))
	return index, err == nil && index >= 0
}
