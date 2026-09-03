// Package selfcert creates and loads a self-signed certificate for
// listeners that need TLS before an admin has issued a real one (syslog over
// TLS). The key pair lives in the data directory and is reused across restarts.
package selfcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Load returns the certificate at <dir>/<name>.crt|.key, creating a 10-year
// self-signed ECDSA P-256 one when missing. Fingerprint is the SHA-256 of the
// DER certificate — what an admin pins on the sending device.
func Load(dir, name, cn string) (cert tls.Certificate, fingerprint string, created bool, err error) {
	crt, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if _, e := os.Stat(crt); e != nil {
		if err = generate(crt, key, cn); err != nil {
			return
		}
		created = true
	}
	cert, err = tls.LoadX509KeyPair(crt, key)
	if err != nil {
		return
	}
	sum := sha256.Sum256(cert.Certificate[0])
	fingerprint = colons(hex.EncodeToString(sum[:]))
	return
}

func generate(crt, key, cn string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"TopoLight"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func colons(h string) string {
	out := make([]byte, 0, len(h)+len(h)/2)
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, h[i], h[i+1])
	}
	return string(out)
}
