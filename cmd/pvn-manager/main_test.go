package main

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pvnconfig "github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/pki"
)

func TestApplyClusterConfigHonorsExplicitFlags(t *testing.T) {
	target := managerConfig{listen: "flag-listen", unixSocket: "old-socket", tlsCert: "old-cert", tlsKey: "old-key", pveAPIURL: "old-pve", clusterName: "old-cluster", webRoot: "old-web", sessionTTL: time.Minute}
	cluster := pvnconfig.Default()
	cluster.Cluster.ID = "cluster-a"
	cluster.Manager.ListenAddress = ":8443"
	cluster.Manager.UnixSocket = "/run/pvn/manager.sock"
	cluster.Manager.WebRoot = "/usr/share/pvn/web"
	cluster.Security.SessionTTL = 20 * time.Minute
	cluster.Security.AllowedOrigins = []string{"https://pve.example:8006"}
	cluster.OVN.ControlDB = []string{"ssl:192.0.2.10:6645"}
	cluster.OVN.Northbound = []string{"ssl:192.0.2.10:6641"}
	cluster.OVN.TLSCA = "/etc/pvn/pki/ca.pem"
	cluster.OVN.TLSCert = "/etc/pvn/pki/node.pem"
	cluster.OVN.TLSKey = "/etc/pvn/pki/node-key.pem"
	applyClusterConfig(&target, cluster, map[string]bool{"listen": true})
	if target.listen != "flag-listen" {
		t.Fatalf("explicit listen overwritten: %q", target.listen)
	}
	if target.unixSocket != cluster.Manager.UnixSocket || target.clusterName != "cluster-a" || target.webRoot != cluster.Manager.WebRoot || target.sessionTTL != 20*time.Minute {
		t.Fatalf("cluster config not applied: %#v", target)
	}
	if len(target.frameAncestors) != 1 || target.frameAncestors[0] != "https://pve.example:8006" {
		t.Fatalf("frame ancestors=%v", target.frameAncestors)
	}
	if len(target.controlDB) != 1 || len(target.northbound) != 1 || target.ovnTLSCA != cluster.OVN.TLSCA || target.reconcileEvery != cluster.Cluster.ReconcileEvery {
		t.Fatalf("OVN settings not applied: %#v", target)
	}
}

func TestLoadMutualTLS(t *testing.T) {
	directory := t.TempDir()
	ca, err := pki.CreateCA(pki.CAOptions{Directory: filepath.Join(directory, "ca"), ClusterID: "manager-test"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := pki.IssueNode(pki.IssueOptions{
		CACertificate: ca.Certificate,
		CAKey:         ca.PrivateKey,
		Directory:     filepath.Join(directory, "node"),
		Name:          "pve-a",
		DNSNames:      []string{"pve-a"},
		IPAddresses:   []net.IP{net.ParseIP("192.0.2.10")},
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := loadMutualTLS(ca.Certificate, node.Certificate, node.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.MinVersion < tls.VersionTLS12 || configuration.RootCAs == nil || len(configuration.Certificates) != 1 {
		t.Fatalf("TLS configuration = %#v", configuration)
	}
}

func TestListenUnixCreatesRestrictedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "manager.sock")
	listener, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode %v is not a socket", info.Mode())
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("socket permissions=%o", info.Mode().Perm())
	}
}

func TestListenUnixRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(path); err == nil {
		t.Fatal("regular file was replaced")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "do not replace" {
		t.Fatalf("regular file changed: %q err=%v", content, err)
	}
}

func TestRunVersionDoesNotRequireRuntimeConfig(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
}
