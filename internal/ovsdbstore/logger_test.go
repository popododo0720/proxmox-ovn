package ovsdbstore

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLibovsdbLoggerKeepsDiagnosticsWithoutPayloadDumps(t *testing.T) {
	var output bytes.Buffer
	logger := newLibovsdbLogger(&output)

	logger.V(3).Info("connection lost, reconnecting", "endpoint", "ssl:control.example:6641")
	logger.V(4).Info("transacting operations", "operations", "tenant-secret-operation")
	logger.V(5).Info("updating model", "new", "tenant-secret-model")
	logger.V(5).Error(errors.New("connection refused"), "failed to reconnect")

	logs := output.String()
	for _, expected := range []string{"connection lost, reconnecting", "failed to reconnect", "connection refused"} {
		if !strings.Contains(logs, expected) {
			t.Errorf("expected diagnostic %q in logger output: %s", expected, logs)
		}
	}
	for _, forbidden := range []string{
		"transacting operations",
		"updating model",
		"tenant-secret-operation",
		"tenant-secret-model",
	} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("verbose payload %q leaked into logger output: %s", forbidden, logs)
		}
	}
}
