package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writePVEMembersFixture(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".members")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeploymentNameFromPVEMembers(t *testing.T) {
	clustered := writePVEMembersFixture(t, `{
  "nodename": "prox2",
  "version": 4,
  "cluster": {"name": "lab-cluster", "version": 4, "nodes": 3, "quorate": 1},
  "nodelist": {
    "prox1": {"id": 1, "online": 1, "ip": "192.0.2.11"},
    "prox2": {"id": 2, "online": 1, "ip": "192.0.2.12"},
    "prox3": {"id": 3, "online": 1, "ip": "192.0.2.13"}
  }
}`)
	name, err := deploymentNameFromPVEMembers(clustered)
	if err != nil {
		t.Fatal(err)
	}
	if name != "lab-cluster" {
		t.Fatalf("clustered deployment name=%q", name)
	}
	nonQuorate := writePVEMembersFixture(t, `{
  "nodename":"prox1", "version":5,
  "cluster":{"name":"maintenance-cluster","version":5,"nodes":1,"quorate":0},
  "nodelist":{"prox1":{"id":1,"online":1}}
}`)
	name, err = deploymentNameFromPVEMembers(nonQuorate)
	if err != nil || name != "maintenance-cluster" {
		t.Fatalf("non-quorate deployment name=%q err=%v", name, err)
	}

	standalone := writePVEMembersFixture(t, `{"nodename":"prox1","version":9}`)
	name, err = deploymentNameFromPVEMembers(standalone)
	if err != nil {
		t.Fatal(err)
	}
	if name != "standalone-prox1" {
		t.Fatalf("standalone deployment name=%q", name)
	}
}

func TestDeploymentNameFromPVEMembersFailsClosed(t *testing.T) {
	fixtures := map[string]string{
		"empty":                  "",
		"root is not object":     `[]`,
		"trailing JSON":          `{"nodename":"prox1","version":1} {}`,
		"duplicate root key":     `{"nodename":"prox1","nodename":"prox2","version":1}`,
		"duplicate cluster name": `{"cluster":{"name":"one","name":"two"}}`,
		"cluster is null":        `{"cluster":null}`,
		"cluster name missing":   `{"cluster":{"quorate":1}}`,
		"cluster name unsafe":    `{"cluster":{"name":"../../etc/passwd"}}`,
		"clustered root extra key": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1},
  "nodelist":{"prox1":{}},"unexpected":true
}`,
		"cluster metadata extra key": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1,"unexpected":true},
  "nodelist":{"prox1":{}}
}`,
		"clustered membership version invalid": `{
  "nodename":"prox1","version":null,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1},
  "nodelist":{"prox1":{}}
}`,
		"cluster node count mismatch": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":2,"quorate":1},
  "nodelist":{"prox1":{}}
}`,
		"cluster local node missing": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1},
  "nodelist":{"prox2":{}}
}`,
		"cluster nodelist empty": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1},
  "nodelist":{}
}`,
		"cluster member malformed": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":1},
  "nodelist":{"prox1":null}
}`,
		"cluster quorate invalid": `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"lab","version":1,"nodes":1,"quorate":2},
  "nodelist":{"prox1":{}}
}`,
		"standalone has extra key":    `{"nodename":"prox1","version":1,"nodelist":{}}`,
		"standalone version float":    `{"nodename":"prox1","version":1.5}`,
		"standalone version string":   `{"nodename":"prox1","version":"1"}`,
		"standalone version negative": `{"nodename":"prox1","version":-1}`,
		"standalone node unsafe":      `{"nodename":"prox 1","version":1}`,
	}
	for name, payload := range fixtures {
		t.Run(name, func(t *testing.T) {
			if _, err := deploymentNameFromPVEMembers(writePVEMembersFixture(t, payload)); err == nil {
				t.Fatal("malformed membership was accepted")
			}
		})
	}

	tooLarge := writePVEMembersFixture(t, strings.Repeat(" ", maxPVEMembersBytes+1))
	if _, err := deploymentNameFromPVEMembers(tooLarge); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized membership error=%v", err)
	}
	tooDeep := writePVEMembersFixture(t, strings.Repeat("[", maxPVEMembersJSONDepth+2)+"0"+strings.Repeat("]", maxPVEMembersJSONDepth+2))
	if _, err := deploymentNameFromPVEMembers(tooDeep); err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("deeply nested membership error=%v", err)
	}

	if _, err := deploymentNameFromPVEMembers(t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory membership error=%v", err)
	}
	realFile := writePVEMembersFixture(t, `{"nodename":"prox1","version":1}`)
	symlink := filepath.Join(t.TempDir(), ".members")
	if err := os.Symlink(realFile, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := deploymentNameFromPVEMembers(symlink); err == nil {
		t.Fatal("membership symlink was accepted")
	}
	pipe := filepath.Join(t.TempDir(), ".members")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deploymentNameFromPVEMembers(pipe); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("FIFO membership error=%v", err)
	}
}

func TestApplyPVEDeploymentNamePrecedenceAndFallback(t *testing.T) {
	clustered := writePVEMembersFixture(t, `{
  "nodename":"prox1","version":1,
  "cluster":{"name":"human-cluster","version":1,"nodes":1,"quorate":1},
  "nodelist":{"prox1":{}}
}`)
	target := managerConfig{clusterName: "6071ee6d-37d0-4932-a9a8-3286fb9349fb", pveMembersFile: clustered}
	if err := applyPVEDeploymentName(&target, false); err != nil {
		t.Fatal(err)
	}
	if target.clusterName != "human-cluster" {
		t.Fatalf("membership name did not replace UUID fallback: %q", target.clusterName)
	}

	target = managerConfig{clusterName: "operator-name", pveMembersFile: filepath.Join(t.TempDir(), "missing")}
	if err := applyPVEDeploymentName(&target, true); err != nil {
		t.Fatalf("explicit cluster-name unexpectedly read membership: %v", err)
	}
	if target.clusterName != "operator-name" {
		t.Fatalf("explicit cluster-name overwritten: %q", target.clusterName)
	}

	target = managerConfig{clusterName: "test-installation-id"}
	if err := applyPVEDeploymentName(&target, false); err != nil {
		t.Fatal(err)
	}
	if target.clusterName != "test-installation-id" {
		t.Fatalf("non-production fallback overwritten: %q", target.clusterName)
	}

	target = managerConfig{clusterName: "must-not-survive", pveMembersFile: filepath.Join(t.TempDir(), "missing")}
	if err := applyPVEDeploymentName(&target, false); err == nil {
		t.Fatal("configured production membership failure was ignored")
	}
}
