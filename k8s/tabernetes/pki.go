package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type pki struct {
	caCert *x509.Certificate
	caKey  crypto.Signer
	caPEM  []byte
}

func p(name string) string { return filepath.Join(dataDir, name) }

func newKey() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	return k
}

func keyPEM(k *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(k)
	must(err)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	must(err)
	return n
}

// newPKI creates a CA and writes it to <name>.crt and <name>.key.
func newPKI(name, cn string) *pki {
	key := newKey()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)
	cert, err := x509.ParseCertificate(der)
	must(err)
	ca := &pki{caCert: cert, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	must(os.WriteFile(p(name+".crt"), ca.caPEM, 0o644))
	must(os.WriteFile(p(name+".key"), keyPEM(key), 0o600))
	return ca
}

// caHash returns the kubeadm style CA public key pin.
func (c *pki) caHash() string {
	sum := sha256.Sum256(c.caCert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (c *pki) issue(name, cn string, orgs []string, server bool, dns []string, ips []net.IP) {
	key := newKey()
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if server {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, &key.PublicKey, c.caKey)
	must(err)
	must(os.WriteFile(p(name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	must(os.WriteFile(p(name+".key"), keyPEM(key), 0o600))
}

func randToken(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	must(err)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func writeKubeconfig(name, token string) {
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: tabernetes
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority: %s
contexts:
- name: tabernetes
  context:
    cluster: tabernetes
    user: %s
current-context: tabernetes
users:
- name: %s
  user:
    token: %s
`, p("ca.crt"), name, name, token)
	must(os.WriteFile(p(name+".kubeconfig"), []byte(cfg), 0o600))
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// loadPKI reads a CA that another tab created (tabs.go).
func loadPKI(certFile, keyFile string) (*pki, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s: not a signing key", keyFile)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &pki{caCert: cert, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})}, nil
}
