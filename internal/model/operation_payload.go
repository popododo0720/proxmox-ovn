package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// MaxOperationPayloadBytes bounds durable internal operation state stored in
// an OVSDB external_ids value.
const MaxOperationPayloadBytes = 32 * 1024

// MarshalOperationPayload encodes a typed value as a deterministic JSON
// object suitable for Operation.Payload.
func MarshalOperationPayload(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal operation payload: %w", err)
	}
	canonical, err := canonicalOperationPayload(encoded)
	if err != nil {
		return "", err
	}
	if len(canonical) > MaxOperationPayloadBytes {
		return "", fmt.Errorf("operation payload exceeds %d bytes", MaxOperationPayloadBytes)
	}
	return string(canonical), nil
}

// UnmarshalOperationPayload validates and strictly decodes a durable payload.
// Unknown fields are rejected when destination is a struct.
func UnmarshalOperationPayload(payload string, destination any) error {
	if destination == nil {
		return fmt.Errorf("operation payload destination is nil")
	}
	if err := ValidateOperationPayload(payload); err != nil {
		return err
	}
	if payload == "" {
		return fmt.Errorf("operation payload is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode operation payload: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode operation payload: %w", err)
	}
	return nil
}

// ValidateOperationPayload accepts an empty payload or a bounded canonical
// JSON object. Canonical form is the compact encoding produced after decoding
// with json.Number preservation; this sorts object keys recursively and
// rejects duplicate keys through the resulting byte comparison.
func ValidateOperationPayload(payload string) error {
	if payload == "" {
		return nil
	}
	if len(payload) > MaxOperationPayloadBytes {
		return invalid("payload", "must not exceed %d bytes", MaxOperationPayloadBytes)
	}
	canonical, err := canonicalOperationPayload([]byte(payload))
	if err != nil {
		return invalid("payload", "%s", err)
	}
	if !bytes.Equal([]byte(payload), canonical) {
		return invalid("payload", "must be a canonical JSON object")
	}
	return nil
}

func canonicalOperationPayload(payload []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("must contain valid JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("must be a JSON object")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return canonical, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("must contain exactly one JSON value")
		}
		return fmt.Errorf("contains trailing data: %w", err)
	}
	return nil
}
