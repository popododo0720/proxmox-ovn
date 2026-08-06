package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const (
	maxPVEMembersBytes     = 1 << 20
	maxPVEMembersJSONDepth = 32
)

var pveDeploymentNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,125}[A-Za-z0-9_-])?$`)

func applyPVEDeploymentName(target *managerConfig, clusterNameExplicit bool) error {
	if clusterNameExplicit || strings.TrimSpace(target.pveMembersFile) == "" {
		return nil
	}
	name, err := deploymentNameFromPVEMembers(target.pveMembersFile)
	if err != nil {
		return fmt.Errorf("derive PVN deployment name: %w", err)
	}
	target.clusterName = name
	return nil
}

func deploymentNameFromPVEMembers(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("PVE membership credential path is required")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("read PVE membership credential: %w", err)
	}
	defer file.Close()
	metadata, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect PVE membership credential: %w", err)
	}
	if !metadata.Mode().IsRegular() {
		return "", errors.New("PVE membership credential must be a regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxPVEMembersBytes+1))
	if err != nil {
		return "", fmt.Errorf("read PVE membership credential: %w", err)
	}
	if len(payload) > maxPVEMembersBytes {
		return "", fmt.Errorf("PVE membership credential exceeds %d bytes", maxPVEMembersBytes)
	}
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return "", fmt.Errorf("decode PVE membership credential: %w", err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		return "", fmt.Errorf("decode PVE membership credential: %w", err)
	}
	if document == nil {
		return "", errors.New("PVE membership credential root must be an object")
	}
	if clusterPayload, clustered := document["cluster"]; clustered {
		if !hasExactJSONKeys(document, "nodename", "version", "cluster", "nodelist") {
			return "", errors.New("clustered PVE membership must contain exactly nodename, version, cluster, and nodelist")
		}
		node, err := safePVEDeploymentName(document["nodename"], "clustered nodename")
		if err != nil {
			return "", err
		}
		if _, err := nonNegativeJSONInteger(document["version"], "membership version", true); err != nil {
			return "", err
		}

		var cluster map[string]json.RawMessage
		if err := json.Unmarshal(clusterPayload, &cluster); err != nil || cluster == nil {
			return "", errors.New("PVE membership cluster must be an object")
		}
		if !hasExactJSONKeys(cluster, "name", "version", "nodes", "quorate") {
			return "", errors.New("PVE membership cluster must contain exactly name, version, nodes, and quorate")
		}
		name, err := safePVEDeploymentName(cluster["name"], "cluster name")
		if err != nil {
			return "", err
		}
		if _, err := nonNegativeJSONInteger(cluster["version"], "cluster version", true); err != nil {
			return "", err
		}
		nodeCount, err := nonNegativeJSONInteger(cluster["nodes"], "cluster node count", false)
		if err != nil {
			return "", err
		}
		quorate, err := nonNegativeJSONInteger(cluster["quorate"], "cluster quorate state", true)
		if err != nil || quorate > 1 {
			return "", errors.New("PVE membership cluster quorate state must be 0 or 1")
		}

		var nodeList map[string]json.RawMessage
		if err := json.Unmarshal(document["nodelist"], &nodeList); err != nil || len(nodeList) == 0 {
			return "", errors.New("PVE membership nodelist must be a nonempty object")
		}
		if nodeCount != int64(len(nodeList)) {
			return "", errors.New("PVE membership cluster node count does not match nodelist")
		}
		if _, localPresent := nodeList[node]; !localPresent {
			return "", errors.New("PVE membership nodelist does not contain the local node")
		}
		for memberName, memberPayload := range nodeList {
			if !pveDeploymentNamePattern.MatchString(memberName) {
				return "", errors.New("PVE membership nodelist contains an unsafe node name")
			}
			var member map[string]json.RawMessage
			if err := json.Unmarshal(memberPayload, &member); err != nil || member == nil {
				return "", fmt.Errorf("PVE membership node %q must be an object", memberName)
			}
		}
		return name, nil
	}

	if len(document) != 2 || document["nodename"] == nil || document["version"] == nil {
		return "", errors.New("standalone PVE membership must contain exactly nodename and version")
	}
	node, err := safePVEDeploymentName(document["nodename"], "standalone nodename")
	if err != nil {
		return "", err
	}
	if _, err := nonNegativeJSONInteger(document["version"], "standalone membership version", true); err != nil {
		return "", err
	}
	return "standalone-" + node, nil
}

func hasExactJSONKeys(document map[string]json.RawMessage, keys ...string) bool {
	if len(document) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := document[key]; !present {
			return false
		}
	}
	return true
}

func nonNegativeJSONInteger(payload json.RawMessage, label string, zeroAllowed bool) (int64, error) {
	var value *int64
	if len(payload) == 0 || json.Unmarshal(payload, &value) != nil || value == nil || *value < 0 || (!zeroAllowed && *value == 0) {
		if zeroAllowed {
			return 0, fmt.Errorf("PVE %s must be a non-negative integer", label)
		}
		return 0, fmt.Errorf("PVE %s must be a positive integer", label)
	}
	return *value, nil
}

func safePVEDeploymentName(payload json.RawMessage, label string) (string, error) {
	var name string
	if len(payload) == 0 || json.Unmarshal(payload, &name) != nil || !pveDeploymentNamePattern.MatchString(name) {
		return "", fmt.Errorf("PVE membership %s is missing or unsafe", label)
	}
	return name, nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("PVE membership credential contains trailing JSON")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxPVEMembersJSONDepth {
		return fmt.Errorf("PVE membership JSON exceeds maximum depth %d", maxPVEMembersJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("PVE membership object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("PVE membership contains duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("PVE membership object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("PVE membership array is not closed")
		}
	default:
		return errors.New("PVE membership contains an unexpected JSON delimiter")
	}
	return nil
}
