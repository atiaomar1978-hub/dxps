package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func key(n int) string { return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", n))) }

func isolate(t *testing.T) string {
	dir := t.TempDir()
	for _, k := range []string{"DXPS_JWT_KEY", "DXPS_MASTER_KEY", "DXPS_PG_APP_DSN", "DXPS_PG_OPS_DSN", "DXPS_PG_OWNER_DSN", "DXPS_KAFKA", "DXPS_HUB_ALLOW", "DXPS_PKI_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("DXPS_RUNTIME", dir)
	t.Setenv("DXPS_ENV_FILE", filepath.Join(dir, "secrets.env"))
	return dir
}

// TC-CFG-001: Secrets load from the runtime env file; explicit environment variables win; lists are trimmed.
func TestLoad(t *testing.T) {
	dir := isolate(t)
	body := "# comment\n\nDXPS_JWT_KEY=" + key(32) + "\nDXPS_MASTER_KEY=" + key(40) + "\nnoequals\nDXPS_PG_APP_DSN=postgres://app\n"
	_ = os.WriteFile(filepath.Join(dir, "secrets.env"), []byte(body), 0o600)
	t.Setenv("DXPS_KAFKA", " a:1 , b:2 ,")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.JWTKey) != 32 || len(c.MasterKey) != 32 || c.PGApp != "postgres://app" || len(c.Kafka) != 2 || c.Kafka[1] != "b:2" ||
		c.PKIDir != filepath.Join(dir, "pki") || c.HubAllow[0] != "localhost:9109" || c.Audience != "dxps-api" {
		t.Fatalf("%+v", c)
	}
}

// TC-CFG-002: Missing, malformed or short keys are fatal configuration errors.
func TestKeyErrors(t *testing.T) {
	for name, env := range map[string][2]string{"missing jwt": {"", key(32)}, "short jwt": {key(8), key(32)}, "bad b64": {"***", key(32)}, "missing master": {key(32), ""}} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if env[0] != "" {
				t.Setenv("DXPS_JWT_KEY", env[0])
			}
			if env[1] != "" {
				t.Setenv("DXPS_MASTER_KEY", env[1])
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	dir := isolate(t)
	_ = os.Mkdir(filepath.Join(dir, "secrets.env"), 0o700)
	if _, err := Load(); err == nil {
		t.Fatal("unreadable env file accepted")
	}
	if defaultRuntime() == "" {
		t.Fatal("no default runtime")
	}
}

// TC-CFG-003: an env file line with an empty key is rejected (the key, never the value, is named in the error).
func TestEnvFileBadKey(t *testing.T) {
	dir := isolate(t)
	p := filepath.Join(dir, "bad.env")
	_ = os.WriteFile(p, []byte(" =topsecret\n"), 0o600)
	err := LoadEnvFile(p)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatal(err)
	}
}
