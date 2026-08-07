package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const agentHealthURL = "http://127.0.0.1:9476/healthz"

type agentHealthPayload struct {
	LastSuccess time.Time `json:"last_success"`
	LastError   string    `json:"last_error"`
	Report      struct {
		Errors int `json:"errors"`
	} `json:"report"`
}

func newAgentHealthClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   2 * time.Second,
	}
}

func probeAgentHealth(ctx context.Context, client *http.Client) error {
	return probeAgentHealthAt(ctx, client, agentHealthURL)
}

func probeAgentHealthAt(ctx context.Context, client *http.Client, endpoint string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("query local PVN agent health: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 1<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, limited)
		return fmt.Errorf("local PVN agent health status is %s", response.Status)
	}
	var health agentHealthPayload
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&health); err != nil {
		return fmt.Errorf("decode local PVN agent health: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode local PVN agent health: response contains trailing JSON")
	}
	if health.LastSuccess.IsZero() {
		return errors.New("local PVN agent has no successful binding scan")
	}
	if health.LastSuccess.After(time.Now().Add(5 * time.Minute)) {
		return errors.New("local PVN agent last-success timestamp is in the future")
	}
	if health.LastError != "" || health.Report.Errors != 0 {
		return errors.New("local PVN agent binding scan contains errors")
	}
	return nil
}
