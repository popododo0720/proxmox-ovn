package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeAgentHealthAcceptsFreshErrorFreeScan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"last_scan":"2026-08-08T00:00:00Z","last_success":"` +
			time.Now().UTC().Format(time.RFC3339Nano) + `","report":{"errors":0,"bound":2}}`))
	}))
	defer server.Close()
	if err := probeAgentHealthAt(context.Background(), server.Client(), server.URL); err != nil {
		t.Fatal(err)
	}
}

func TestProbeAgentHealthRejectsUnhealthyOrInvalidResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "agent status", status: http.StatusServiceUnavailable, body: `{}`},
		{name: "scan errors", status: http.StatusOK, body: `{"last_success":"2026-08-08T00:00:00Z","report":{"errors":1}}`},
		{name: "no success", status: http.StatusOK, body: `{"report":{"errors":0}}`},
		{name: "invalid json", status: http.StatusOK, body: `{broken`},
		{name: "trailing json", status: http.StatusOK, body: `{"last_success":"2026-08-08T00:00:00Z","report":{"errors":0}} {}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = writer.Write([]byte(testCase.body))
			}))
			defer server.Close()
			err := probeAgentHealthAt(context.Background(), server.Client(), server.URL)
			if err == nil || strings.TrimSpace(err.Error()) == "" {
				t.Fatalf("unhealthy response accepted: status=%d body=%s", testCase.status, testCase.body)
			}
		})
	}
}
