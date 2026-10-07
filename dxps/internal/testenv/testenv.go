// Package testenv provides fixtures shared by DxPS tests: the seed NE registry, an in-memory PKI and
// access to the local PostgreSQL / Kafka runtime for integration tests (skipped when unavailable).
package testenv

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dxps/internal/config"
	"dxps/internal/pki"
	"dxps/internal/registry"
	"dxps/internal/seed"
)

// Registry returns the seed NE registry with endpoints pointing at host (ids = codes).
func Registry(host string) *registry.Registry {
	var nes []*registry.NE
	for _, n := range seed.NEs(host) {
		ne := n.NE
		ne.ID = ne.Code
		ne.Access = map[string]registry.Access{}
		for _, a := range n.Access {
			ne.Access[a.Tenant] = a
		}
		nes = append(nes, &ne)
	}
	return registry.New(nes...)
}

// RegistryWithBase is Registry with every endpoint replaced by base (single httptest server).
func RegistryWithBase(base string) *registry.Registry {
	r := Registry("localhost")
	for _, ne := range r.All() {
		for i := range ne.Endpoints {
			ne.Endpoints[i].BaseURI = base
		}
	}
	return r
}

type PKI struct {
	CA     *pki.CA
	CAPEM  []byte
	Server *pki.Bundle
	Client *pki.Bundle
}

func NewPKI(t testing.TB) *PKI {
	t.Helper()
	ca, err := pki.NewCA("dxps-test-ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ca.Issue(pki.LeafOpts{CommonName: "netsim", DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ca.Issue(pki.LeafOpts{CommonName: "adapter", Client: true})
	if err != nil {
		t.Fatal(err)
	}
	return &PKI{CA: ca, CAPEM: ca.CertPEM, Server: srv, Client: cl}
}

func (p *PKI) ServerTLS(t testing.TB, mtls bool) *tls.Config {
	var ca []byte
	if mtls {
		ca = p.CAPEM
	}
	c, err := pki.ServerTLS(p.Server, ca)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (p *PKI) ClientTLS(t testing.TB, withCert bool) *tls.Config {
	var b *pki.Bundle
	if withCert {
		b = p.Client
	}
	c, err := pki.ClientTLS(p.CAPEM, b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Config loads the local runtime configuration or skips the test (integration tests need
// PostgreSQL and Kafka from scripts/infra-setup.ps1). Secrets stay inside the test process.
func Config(t testing.TB) *config.Config {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	rt := os.Getenv("DXPS_RUNTIME")
	if rt == "" {
		h, _ := os.UserHomeDir()
		rt = filepath.Join(h, "dxps-runtime")
	}
	if _, err := os.Stat(filepath.Join(rt, "secrets.env")); err != nil { // #nosec G703 -- test helper, developer env
		t.Skip("DxPS runtime not installed")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skip("runtime config: ", err)
	}
	return cfg
}

// Kafka returns the brokers when Kafka is reachable or skips.
func Kafka(t testing.TB) []string {
	cfg := Config(t)
	c, err := net.DialTimeout("tcp", cfg.Kafka[0], time.Second)
	if err != nil {
		t.Skip("kafka unavailable")
	}
	c.Close()
	return cfg.Kafka
}
