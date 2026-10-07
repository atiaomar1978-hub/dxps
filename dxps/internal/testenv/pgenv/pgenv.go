// Package pgenv opens the local DxPS PostgreSQL for integration tests (skipped when unavailable).
package pgenv

import (
	"context"
	"testing"
	"time"

	"dxps/internal/config"
	"dxps/internal/store"
	"dxps/internal/testenv"
)

// Store opens the app (RLS) and ops pools against the local PostgreSQL or skips.
func Store(t testing.TB) (*store.Store, *config.Config) {
	cfg := testenv.Config(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := store.Open(ctx, cfg.PGApp, cfg.PGOps)
	if err != nil {
		t.Skip("postgres unavailable: ", err)
	}
	t.Cleanup(s.Close)
	return s, cfg
}
