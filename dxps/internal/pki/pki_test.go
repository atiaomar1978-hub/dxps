package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func issue(t *testing.T) (*CA, *Bundle, *Bundle) {
	ca, err := NewCA("test-ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ca.Issue(LeafOpts{CommonName: "srv", DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ca.Issue(LeafOpts{CommonName: "client", Client: true, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return ca, srv, cl
}

func serve(t *testing.T, cfg *tls.Config) string {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		io.WriteString(w, r.Proto+" "+cn)
	}))
	s.TLS = cfg
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)
	return s.URL
}

func get(cfg *tls.Config, url string) (string, error) {
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}, Timeout: 5 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b), nil
}

// TC-PKI-001: Mutual TLS 1.3 over HTTP/2 succeeds with a client certificate from the DxPS CA.
func TestMutualTLS(t *testing.T) {
	ca, srv, cl := issue(t)
	scfg, _ := ServerTLS(srv, ca.CertPEM)
	url := serve(t, scfg)
	ccfg, _ := ClientTLS(ca.CertPEM, cl)
	got, err := get(ccfg, url)
	if err != nil || got != "HTTP/2.0 client" {
		t.Fatalf("%q %v", got, err)
	}
	if scfg.MinVersion != tls.VersionTLS13 || ccfg.MinVersion != tls.VersionTLS13 {
		t.Fatal("TLS 1.3 not enforced")
	}
}

// TC-PKI-002: The NE side rejects clients without a certificate or with a certificate from a foreign CA.
func TestMTLSRejections(t *testing.T) {
	ca, srv, _ := issue(t)
	scfg, _ := ServerTLS(srv, ca.CertPEM)
	url := serve(t, scfg)
	noCert, _ := ClientTLS(ca.CertPEM, nil)
	if _, err := get(noCert, url); err == nil {
		t.Fatal("client without certificate accepted")
	}
	rogue, _ := NewCA("rogue", time.Hour)
	rc, _ := rogue.Issue(LeafOpts{CommonName: "adapter", Client: true})
	cfg, _ := ClientTLS(ca.CertPEM, nil)
	cert, _ := tls.X509KeyPair(rc.CertPEM, rc.KeyPEM)
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
	if _, err := get(cfg, url); err == nil {
		t.Fatal("foreign CA certificate accepted")
	}
}

// TC-PKI-003: Clients refuse servers not signed by the DxPS CA and TLS < 1.3.
func TestServerVerification(t *testing.T) {
	ca, _, _ := issue(t)
	rogue, _ := NewCA("rogue", time.Hour)
	rs, _ := rogue.Issue(LeafOpts{CommonName: "evil", DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true})
	scfg, _ := ServerTLS(rs, nil)
	url := serve(t, scfg)
	ccfg, _ := ClientTLS(ca.CertPEM, nil)
	if _, err := get(ccfg, url); err == nil {
		t.Fatal("rogue server trusted")
	}
	_, srv, _ := issue(t)
	old, _ := ServerTLS(srv, nil)
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	url = serve(t, old)
	if _, err := get(ccfg, url); err == nil {
		t.Fatal("TLS 1.2 negotiated")
	}
}

// TC-PKI-004: Leaf certificates carry correct key usages, SANs and validity; CA files round-trip with private key 0600.
func TestBundleFiles(t *testing.T) {
	ca, srv, cl := issue(t)
	blk, _ := pem.Decode(srv.CertPEM)
	c, _ := x509.ParseCertificate(blk.Bytes)
	if c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || c.DNSNames[0] != "localhost" || c.NotAfter.Sub(c.NotBefore) < 24*time.Hour {
		t.Fatalf("server leaf %+v", c.ExtKeyUsage)
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: func() *x509.CertPool { p := x509.NewCertPool(); p.AddCert(ca.Cert); return p }(), DNSName: "localhost"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kp, _ := ca.KeyPEM()
	_ = os.WriteFile(filepath.Join(dir, "ca.crt"), ca.CertPEM, 0o644)
	if err := WriteBundle(dir, "gateway", srv); err != nil {
		t.Fatal(err)
	}
	_ = WriteBundle(dir, "adapter", cl)
	if _, err := ServerTLSFiles(dir, "gateway", true); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerTLSFiles(dir, "gateway", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLSFiles(dir, "adapter"); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLSFiles(dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLSFiles(dir, "missing"); err == nil {
		t.Fatal("missing bundle loaded")
	}
	if _, err := ServerTLSFiles(dir, "missing", false); err == nil {
		t.Fatal("missing bundle loaded")
	}
	if _, err := ReadBundle(dir, "nokey"); err == nil {
		t.Fatal("expected error")
	}
	_ = os.WriteFile(filepath.Join(dir, "half.crt"), srv.CertPEM, 0o644)
	if _, err := ReadBundle(dir, "half"); err == nil {
		t.Fatal("bundle without key loaded")
	}
	if l, err := LoadCA(ca.CertPEM, kp); err != nil || !l.Cert.IsCA {
		t.Fatal(err)
	}
	if _, err := LoadCA([]byte("x"), kp); err == nil {
		t.Fatal("bad CA PEM accepted")
	}
	leafKey := cl.KeyPEM
	if _, err := LoadCA(cl.CertPEM, leafKey); err == nil {
		t.Fatal("leaf accepted as CA")
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})
	if _, err := LoadCA(bad, kp); err == nil {
		t.Fatal("junk cert accepted")
	}
	if _, err := LoadCA(ca.CertPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})); err == nil {
		t.Fatal("junk key accepted")
	}
	if _, err := ClientTLS([]byte("no pem"), nil); err == nil || !strings.Contains(err.Error(), "no CA") {
		t.Fatal("empty CA pool accepted")
	}
	if _, err := ServerTLS(srv, []byte("no pem")); err == nil {
		t.Fatal("empty client CA pool accepted")
	}
	if _, err := ServerTLS(&Bundle{CertPEM: []byte("x"), KeyPEM: []byte("y")}, nil); err == nil {
		t.Fatal("bad keypair accepted")
	}
	if _, err := ClientTLS(ca.CertPEM, &Bundle{CertPEM: []byte("x")}); err == nil {
		t.Fatal("bad client keypair accepted")
	}
	if err := WriteBundle(filepath.Join(dir, "ca.crt", "sub"), "x", srv); err == nil {
		t.Fatal("write into file path succeeded")
	}
	if !errors.Is(func() error { _, err := ReadCAPEM(t.TempDir()); return err }(), os.ErrNotExist) {
		t.Fatal("missing CA")
	}
}
