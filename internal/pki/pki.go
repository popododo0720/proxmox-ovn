package pki

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type Files struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

type CAOptions struct {
	Directory string
	ClusterID string
	Now       func() time.Time
}

func CreateCA(options CAOptions) (Files, error) {
	if options.Directory == "" || options.ClusterID == "" {
		return Files{}, errors.New("CA directory and cluster ID are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	certificatePath := filepath.Join(options.Directory, "ca.pem")
	keyPath := filepath.Join(options.Directory, "ca-key.pem")
	if err := pathsDoNotExist(certificatePath, keyPath); err != nil {
		return Files{}, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Files{}, fmt.Errorf("generate CA key: %w", err)
	}
	now := options.Now().UTC()
	serial, err := randomSerial()
	if err != nil {
		return Files{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PVN " + options.ClusterID + " CA", Organization: []string{"PVN"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return Files{}, fmt.Errorf("create CA certificate: %w", err)
	}
	if err := writePair(options.Directory, certificatePath, keyPath, der, privateKey); err != nil {
		return Files{}, err
	}
	return Files{Certificate: certificatePath, PrivateKey: keyPath}, nil
}

type IssueOptions struct {
	CACertificate string
	CAKey         string
	Directory     string
	Name          string
	DNSNames      []string
	IPAddresses   []net.IP
	Now           func() time.Time
}

func IssueNode(options IssueOptions) (Files, error) {
	if options.CACertificate == "" || options.CAKey == "" || options.Directory == "" || options.Name == "" {
		return Files{}, errors.New("CA certificate, CA key, output directory, and node name are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if !safeName(options.Name) {
		return Files{}, errors.New("node name contains unsupported characters")
	}
	caCertificate, caPrivateKey, err := loadCA(options.CACertificate, options.CAKey)
	if err != nil {
		return Files{}, err
	}
	certificatePath := filepath.Join(options.Directory, options.Name+".pem")
	keyPath := filepath.Join(options.Directory, options.Name+"-key.pem")
	if err := pathsDoNotExist(certificatePath, keyPath); err != nil {
		return Files{}, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Files{}, fmt.Errorf("generate node key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Files{}, err
	}
	now := options.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: options.Name, Organization: []string{"PVN"}},
		DNSNames:     append([]string(nil), options.DNSNames...),
		IPAddresses:  append([]net.IP(nil), options.IPAddresses...),
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCertificate, publicKey, caPrivateKey)
	if err != nil {
		return Files{}, fmt.Errorf("create node certificate: %w", err)
	}
	if err := writePair(options.Directory, certificatePath, keyPath, der, privateKey); err != nil {
		return Files{}, err
	}
	return Files{Certificate: certificatePath, PrivateKey: keyPath}, nil
}

func loadCA(certificatePath, keyPath string) (*x509.Certificate, ed25519.PrivateKey, error) {
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA certificate: %w", err)
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("CA certificate is not valid PEM")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil || !certificate.IsCA {
		return nil, nil, errors.New("CA certificate is invalid or is not a CA")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, nil, errors.New("CA key is not valid PKCS#8 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, errors.New("CA key is not Ed25519")
	}
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(publicKey, privateKey.Public().(ed25519.PublicKey)) {
		return nil, nil, errors.New("CA certificate and private key do not match")
	}
	return certificate, privateKey, nil
}

func writePair(directory, certificatePath, keyPath string, certificateDER []byte, privateKey ed25519.PrivateKey) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create PKI directory: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}
	if err := writeExclusive(keyPath, 0o600, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	if err := writeExclusive(certificatePath, 0o644, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})); err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	return nil
}

func writeExclusive(path string, mode os.FileMode, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync %q: %w", path, err)
	}
	return file.Close()
}

func pathsDoNotExist(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %q", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %q: %w", path, err)
		}
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func safeName(value string) bool {
	if value == "" || len(value) > 127 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}
