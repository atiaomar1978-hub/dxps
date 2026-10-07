package svc

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dxps/internal/testenv"
)

// TC-SVC-001: Boot loads configuration, writes JSON logs to $DXPS_LOG_DIR/<name>.log (0600) and returns a
// cancellable context.
func TestBoot(t *testing.T) {
	// The log file stays open for the process lifetime (as in the services), so t.TempDir cleanup would fail on Windows.
	dir, err := os.MkdirTemp("", "dxps-svc-test")
	if err != nil {
		t.Fatal(err)
	}
	k := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	env := filepath.Join(dir, "secrets.env")
	os.WriteFile(env, []byte("DXPS_JWT_KEY="+k+"\nDXPS_MASTER_KEY="+k+"\n"), 0o600)
	t.Setenv("DXPS_RUNTIME", dir)
	t.Setenv("DXPS_ENV_FILE", env)
	t.Setenv("DXPS_JWT_KEY", "")
	t.Setenv("DXPS_MASTER_KEY", "")
	os.Unsetenv("DXPS_JWT_KEY")
	os.Unsetenv("DXPS_MASTER_KEY")
	t.Setenv("DXPS_LOG_DIR", dir)
	prev := slog.Default()
	defer slog.SetDefault(prev)
	ctx, cancel, cfg, log := Boot("unit")
	if cfg == nil || len(cfg.JWTKey) != 32 || log == nil {
		t.Fatal("boot")
	}
	log.Info("hello", "k", "v")
	cancel()
	<-ctx.Done()
	var b []byte
	b, err = os.ReadFile(filepath.Join(dir, "unit.log"))
	if err != nil || !strings.Contains(string(b), `"svc":"unit"`) || !strings.Contains(string(b), `"msg":"hello"`) {
		t.Fatal(string(b), err)
	}
	if strings.Contains(string(b), k) {
		t.Fatal("secret written to log")
	}
}

// TC-SVC-002: HTTP servers get conservative timeouts and header limits (slowloris protection).
func TestServer(t *testing.T) {
	s := Server("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 || s.MaxHeaderBytes != 32<<10 {
		t.Fatalf("%+v", s)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ln) }()
	go Shutdown(ctx, s)
	cancel()
	if err := <-done; !Ignore(err) {
		t.Fatal(err)
	}
	if !Ignore(nil) || !Ignore(http.ErrServerClosed) || Ignore(errors.New("x")) {
		t.Fatal("Ignore")
	}
}

// TC-SVC-003 (PostgreSQL): OpenStore connects both pools.
func TestOpenStore(t *testing.T) {
	cfg := testenv.Config(t)
	s := OpenStore(context.Background(), cfg, slog.Default())
	defer s.Close()
	if s.App == nil || s.Ops == nil {
		t.Fatal("pools")
	}
}
