// Package pki creates the DxPS development CA, issues ECDSA P-256 leaf certificates and builds
// TLS 1.3 configurations (server, client, mutual TLS).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

type Bundle struct {
	CertPEM []byte
	KeyPEM  []byte
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func NewCA(cn string, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"DxPS"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// LoadCA parses a CA certificate and PKCS#8 key.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("invalid CA PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || !cert.IsCA {
		return nil, errors.New("CA must be an ECDSA CA certificate")
	}
	return &CA{Cert: cert, Key: ek, CertPEM: certPEM}, nil
}

type LeafOpts struct {
	CommonName string
	DNS        []string
	IPs        []net.IP
	Server     bool
	Client     bool
	Validity   time.Duration
}

func (ca *CA) Issue(o LeafOpts) (*Bundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	if o.Validity == 0 {
		o.Validity = 397 * 24 * time.Hour
	}
	var eku []x509.ExtKeyUsage
	if o.Server {
		eku = append(eku, x509.ExtKeyUsageServerAuth)
	}
	if o.Client {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: o.CommonName, Organization: []string{"DxPS"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(o.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
		DNSNames:     o.DNS,
		IPAddresses:  o.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}),
	}, nil
}

func pool(caPEM []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificates found")
	}
	return p, nil
}

// ServerTLS returns a TLS 1.3 server config. With clientCAPEM set, client certificates are required (mTLS).
func ServerTLS(b *Bundle, clientCAPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, err
	}
	c := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if clientCAPEM != nil {
		p, err := pool(clientCAPEM)
		if err != nil {
			return nil, err
		}
		c.ClientCAs = p
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, nil
}

// ClientTLS returns a TLS 1.3 client config trusting caPEM, presenting b when non-nil.
func ClientTLS(caPEM []byte, b *Bundle) (*tls.Config, error) {
	p, err := pool(caPEM)
	if err != nil {
		return nil, err
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p, NextProtos: []string{"h2", "http/1.1"}}
	if b != nil {
		cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
		if err != nil {
			return nil, err
		}
		c.Certificates = []tls.Certificate{cert}
	}
	return c, nil
}

// --- file helpers (runtime/pki/<name>.crt|.key) ---

func WriteBundle(dir, name string, b *Bundle) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), b.CertPEM, 0o644); err != nil { // #nosec G306 -- public certificate; the key below is 0600
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".key"), b.KeyPEM, 0o600)
}

func ReadBundle(dir, name string) (*Bundle, error) {
	c, err := os.ReadFile(filepath.Join(dir, name+".crt")) // #nosec G304 -- runtime PKI dir from config
	if err != nil {
		return nil, err
	}
	k, err := os.ReadFile(filepath.Join(dir, name+".key")) // #nosec G304 -- runtime PKI dir from config
	if err != nil {
		return nil, err
	}
	return &Bundle{CertPEM: c, KeyPEM: k}, nil
}

func ReadCAPEM(dir string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, "ca.crt")) } // #nosec G304 -- runtime PKI dir

// ServerTLSFiles / ClientTLSFiles load bundles from the PKI dir.
func ServerTLSFiles(dir, name string, mtls bool) (*tls.Config, error) {
	b, err := ReadBundle(dir, name)
	if err != nil {
		return nil, err
	}
	var ca []byte
	if mtls {
		if ca, err = ReadCAPEM(dir); err != nil {
			return nil, err
		}
	}
	return ServerTLS(b, ca)
}

func ClientTLSFiles(dir, name string) (*tls.Config, error) {
	ca, err := ReadCAPEM(dir)
	if err != nil {
		return nil, err
	}
	var b *Bundle
	if name != "" {
		if b, err = ReadBundle(dir, name); err != nil {
			return nil, err
		}
	}
	return ClientTLS(ca, b)
}
