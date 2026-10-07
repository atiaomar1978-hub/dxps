// Package config loads DxPS settings from the environment and an optional KEY=VALUE env file.
package config

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Runtime    string // runtime directory (pki, logs, results)
	PKIDir     string
	Kafka      []string
	PGApp      string // dxps_app (RLS enforced)
	PGOps      string // dxps_ops (BYPASSRLS; relay, registry loader, dashboard)
	PGOwner    string // dxps_owner (migrations)
	JWTKey     []byte
	MasterKey  []byte
	Issuer     string
	Audience   string
	GatewayURL string
	NetsimHost string
	HubAllow   []string
}

// LoadEnvFile sets variables from a KEY=VALUE file without overriding variables already set.
func LoadEnvFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied runtime path, not request input
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, strings.TrimSpace(v)); err != nil {
				return fmt.Errorf("env file: invalid key %q: %w", k, err)
			}
		}
	}
	return sc.Err()
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decodeKey(name string, min int) ([]byte, error) {
	v := os.Getenv(name)
	if v == "" {
		return nil, fmt.Errorf("%s not set", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(b) < min {
		return nil, fmt.Errorf("%s must be at least %d bytes", name, min)
	}
	return b, nil
}

// Load reads configuration. DXPS_ENV_FILE (default <runtime>/secrets.env) is loaded first if present.
func Load() (*Config, error) {
	rt := get("DXPS_RUNTIME", defaultRuntime())
	envFile := get("DXPS_ENV_FILE", filepath.Join(rt, "secrets.env"))
	if err := LoadEnvFile(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c := &Config{
		Runtime:    rt,
		PKIDir:     get("DXPS_PKI_DIR", filepath.Join(rt, "pki")),
		Kafka:      splitList(get("DXPS_KAFKA", "127.0.0.1:9092")),
		PGApp:      os.Getenv("DXPS_PG_APP_DSN"),
		PGOps:      os.Getenv("DXPS_PG_OPS_DSN"),
		PGOwner:    os.Getenv("DXPS_PG_OWNER_DSN"),
		Issuer:     get("DXPS_JWT_ISSUER", "https://auth.dxps.local"),
		Audience:   get("DXPS_JWT_AUDIENCE", "dxps-api"),
		GatewayURL: get("DXPS_GATEWAY_URL", "https://localhost:8443"),
		NetsimHost: get("DXPS_NETSIM_HOST", "localhost"),
		HubAllow:   splitList(get("DXPS_HUB_ALLOW", "localhost:9109")),
	}
	var err error
	if c.JWTKey, err = decodeKey("DXPS_JWT_KEY", 32); err != nil {
		return nil, err
	}
	if c.MasterKey, err = decodeKey("DXPS_MASTER_KEY", 32); err != nil {
		return nil, err
	}
	c.MasterKey = c.MasterKey[:32]
	return c, nil
}

func defaultRuntime() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "dxps-runtime")
	}
	return "dxps-runtime"
}
