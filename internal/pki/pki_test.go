package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateCAAndIssueNode(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	ca, err := CreateCA(CAOptions{Directory: filepath.Join(dir, "ca"), ClusterID: "cluster-a", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	node, err := IssueNode(IssueOptions{
		CACertificate: ca.Certificate,
		CAKey:         ca.PrivateKey,
		Directory:     filepath.Join(dir, "nodes"),
		Name:          "pve-a",
		DNSNames:      []string{"pve-a"},
		IPAddresses:   []net.IP{net.ParseIP("192.0.2.10")},
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(node.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(encoded)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Subject.CommonName != "pve-a" || !certificate.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Fatalf("unexpected node certificate: %+v", certificate)
	}
	keyInfo, err := os.Stat(node.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode is %o", keyInfo.Mode().Perm())
	}
	if _, err := CreateCA(CAOptions{Directory: filepath.Join(dir, "ca"), ClusterID: "cluster-a"}); err == nil {
		t.Fatal("CA overwrite should be rejected")
	}
}
