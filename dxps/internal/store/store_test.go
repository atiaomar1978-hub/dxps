package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dxps/internal/catalog"
	"dxps/internal/registry"
	"dxps/internal/seed"
	"dxps/internal/store"
	"dxps/internal/tenant"
	"dxps/internal/testenv/pgenv"
)

const (
	tA = "zz-test-sa"
	tB = "zz-test-sb"
)

// TC-STO-001: outbox shard is stable and within range; error classifiers.
func TestShardAndErrors(t *testing.T) {
	if store.Shard("a|b") != store.Shard("a|b") {
		t.Fatal("unstable shard")
	}
	for _, k := range []string{"", "x", "mvno-alpha|supi:imsi-1", strings.Repeat("z", 500)} {
		if s := store.Shard(k); s < 0 || s >= store.OutboxShards {
			t.Fatal(k, s)
		}
	}
	if !store.Retryable(&pgconn.PgError{Code: "40001"}) || !store.Retryable(&pgconn.PgError{Code: "40P01"}) || store.Retryable(errors.New("x")) {
		t.Fatal("Retryable")
	}
	if !store.BCTerminal(store.BCCompleted) || store.BCTerminal(store.BCInProgress) {
		t.Fatal("BCTerminal")
	}
	if (&store.Task{State: store.TaskReady}).Terminal() || !(&store.Task{State: store.TaskSkipped}).Terminal() {
		t.Fatal("Task.Terminal")
	}
}

func insertHub(t *testing.T, s *store.Store, ten string) *store.Hub {
	t.Helper()
	h := &store.Hub{Tenant: ten, Callback: "https://localhost:9109/hooks/" + ten, SecretRef: "enc:v1:test"}
	if err := s.InTenant(context.Background(), ten, func(tx pgx.Tx) error { return store.InsertHub(context.Background(), tx, h) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.InTenant(context.Background(), ten, func(tx pgx.Tx) error { return store.DeleteHub(context.Background(), tx, h.ID) })
	})
	return h
}

// TC-STO-002 (PostgreSQL RLS): a tenant transaction sees only its own rows even with an explicit WHERE on another
// tenant, cannot insert rows for another tenant (WITH CHECK), and cannot delete another tenant's rows.
func TestRowLevelSecurity(t *testing.T) {
	s, _ := pgenv.Store(t)
	ctx := context.Background()
	ha := insertHub(t, s, tA)
	err := s.InTenant(ctx, tB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM hub_subscription WHERE tenant = $1`, tA).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("RLS leak: tenant B sees tenant A rows")
		}
		hs, err := store.ListHubs(ctx, tx)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.ID == ha.ID {
				t.Fatal("ListHubs leak")
			}
		}
		if err := store.DeleteHub(ctx, tx, ha.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("cross-tenant delete", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.InTenant(ctx, tB, func(tx pgx.Tx) error {
		return store.InsertHub(ctx, tx, &store.Hub{Tenant: tA, Callback: "https://localhost:9109/x", SecretRef: "enc:v1:x"})
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatal("WITH CHECK must reject a row for another tenant, got", err)
	}
}

// TC-STO-003 (PostgreSQL RLS): without the tenant setting the app role sees nothing; invalid tenant ids are refused
// before any SQL runs.
func TestNoTenantContext(t *testing.T) {
	s, _ := pgenv.Store(t)
	ctx := context.Background()
	insertHub(t, s, tA)
	var n int
	if err := s.App.QueryRow(ctx, `SELECT count(*) FROM hub_subscription`).Scan(&n); err != nil || n != 0 {
		t.Fatal("app role without app.tenant must see no rows:", n, err)
	}
	for _, bad := range []string{"", "a b", "x'; DROP TABLE outbox;--", strings.Repeat("a", 41)} {
		if err := s.InTenant(ctx, bad, func(pgx.Tx) error { t.Fatal("fn must not run"); return nil }); !errors.Is(err, tenant.ErrInvalidID) {
			t.Fatal(bad, err)
		}
	}
}

// TC-STO-004 (PostgreSQL): least privilege: the app role cannot update/delete the append-only audit log or touch
// tables it has no grant on.
func TestLeastPrivilege(t *testing.T) {
	s, _ := pgenv.Store(t)
	ctx := context.Background()
	err := s.InTenant(ctx, tA, func(tx pgx.Tx) error {
		return store.InsertAudit(ctx, tx, tA, store.Audit{Actor: "test", Action: "test.write", ObjectType: "test", Detail: map[string]any{"k": 1}})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE audit_event SET action = 'tampered' WHERE tenant = current_tenant()`,
		`DELETE FROM audit_event WHERE tenant = current_tenant()`,
		`TRUNCATE outbox`,
		`DROP TABLE service_order`,
		`ALTER TABLE service_order DISABLE ROW LEVEL SECURITY`,
		// row_security=off does not bypass RLS for the app role: a filtered query errors instead.
		`SET LOCAL row_security = off; SELECT count(*) FROM hub_subscription`,
	} {
		err := s.InTenant(ctx, tA, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if err == nil {
			t.Fatal("app role allowed:", q)
		}
	}
}

// TC-STO-005 (PostgreSQL): outbox rows get ids; MarkPublished is tenant-scoped and idempotent; relay propagates
// publish failures without marking rows.
func TestOutbox(t *testing.T) {
	s, _ := pgenv.Store(t)
	ctx := context.Background()
	rows := []*store.OutboxRow{{Tenant: tA, Topic: "dxps.test", Key: tA + "|k1", Payload: []byte(`{}`), Headers: map[string]string{"tenant": tA}},
		{Tenant: tA, Topic: "dxps.test", Key: tA + "|k2", Payload: []byte(`{}`)}}
	if err := s.InTenant(ctx, tA, func(tx pgx.Tx) error { return store.InsertOutbox(ctx, tx, rows) }); err != nil {
		t.Fatal(err)
	}
	if rows[0].ID == "" || rows[1].ID == "" || rows[0].ID == rows[1].ID {
		t.Fatal("ids not filled")
	}
	boom := errors.New("kafka down")
	if _, err := s.RelayBatch(ctx, 0, 500, func([]*store.OutboxRow) error { return boom }); !errors.Is(err, boom) {
		t.Fatal("relay must propagate publish errors", err)
	}
	if err := s.MarkPublished(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPublished(ctx, rows); err != nil {
		t.Fatal("idempotent", err)
	}
	if err := (&store.Store{}).MarkPublished(ctx, rows); err != nil {
		t.Fatal("no ops pool", err)
	}
	if err := s.InTenant(ctx, tA, func(tx pgx.Tx) error { return store.InsertOutbox(ctx, tx, nil) }); err != nil {
		t.Fatal(err)
	}
}

// TC-STO-006 (PostgreSQL ops role): registry and tenant loaders, statistics for the dashboard.
func TestOpsLoaders(t *testing.T) {
	s, _ := pgenv.Store(t)
	ctx := context.Background()
	ts, err := s.LoadTenants(ctx)
	if err != nil || len(ts) < 5 {
		t.Fatal(len(ts), err)
	}
	nes, err := s.LoadNEs(ctx)
	if err != nil || len(nes) < 11 {
		t.Fatal(len(nes), err)
	}
	shared := 0
	for _, ne := range nes {
		if ne.Shared && len(ne.Access) > 0 {
			shared++
		}
		if len(ne.Endpoints) == 0 {
			t.Fatal("NE without endpoint", ne.Code)
		}
	}
	if shared == 0 {
		t.Fatal("no shared NE with tenant access")
	}
	st, err := s.Stats(ctx)
	if err != nil || st == nil {
		t.Fatal(err)
	}
	if _, err := (&store.Store{}).LoadTenants(ctx); err == nil {
		t.Fatal("ops loaders need the ops pool")
	}
}

// TC-STO-007 (PostgreSQL owner role): migrations are versioned and idempotent; a re-run applies nothing and
// a bad DSN fails cleanly.
func TestMigrateIdempotent(t *testing.T) {
	_, cfg := pgenv.Store(t)
	if cfg.PGOwner == "" {
		t.Skip("owner DSN not configured")
	}
	ctx := context.Background()
	applied, err := store.Migrate(ctx, cfg.PGOwner)
	if err != nil || len(applied) != 0 {
		t.Fatal("re-run must be a no-op", applied, err)
	}
	if _, err := store.Migrate(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"); err == nil {
		t.Fatal("bad DSN accepted")
	}
}

// TC-STO-008 (PostgreSQL ops role): re-applying the seed (tenants, NEs + endpoints, shared-NE access, catalog
// specs) is idempotent: same tenants, same NE ids, same access grants. Seed functions need the ops pool.
func TestSeedIdempotent(t *testing.T) {
	s, cfg := pgenv.Store(t)
	ctx := context.Background()
	snapshot := func() (map[string]string, int, int) {
		ts, err := s.LoadTenants(ctx)
		if err != nil {
			t.Fatal(err)
		}
		nes, err := s.LoadNEs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids, acc := map[string]string{}, 0
		for _, ne := range nes {
			ids[ne.Tenant+"/"+ne.Code] = ne.ID
			acc += len(ne.Access)
		}
		return ids, len(ts), acc
	}
	ids0, nt0, acc0 := snapshot()
	for _, tn := range seed.Tenants() {
		if err := s.UpsertTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range seed.NEs(cfg.NetsimHost) {
		ne := n.NE
		id, err := s.UpsertNE(ctx, &ne, "vault://dxps/"+ne.Tenant+"/ne/"+ne.Code)
		if err != nil {
			t.Fatal(ne.Code, err)
		}
		if want := ids0[ne.Tenant+"/"+ne.Code]; want != "" && want != id {
			t.Fatalf("NE %s id changed %s -> %s", ne.Code, want, id)
		}
		for _, a := range n.Access {
			if err := s.UpsertAccess(ctx, ne.Tenant, id, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range cat.All() {
		if err := s.UpsertSpec(ctx, sp); err != nil {
			t.Fatal(sp.Code, err)
		}
	}
	ids1, nt1, acc1 := snapshot()
	if nt1 != nt0 || acc1 != acc0 || len(ids1) != len(ids0) {
		t.Fatalf("seed not idempotent: tenants %d->%d access %d->%d NEs %d->%d", nt0, nt1, acc0, acc1, len(ids0), len(ids1))
	}
	no := &store.Store{}
	if no.UpsertTenant(ctx, tenant.Tenant{}) == nil || no.UpsertAccess(ctx, "", "", registry.Access{}) == nil ||
		no.UpsertSpec(ctx, &catalog.Spec{}) == nil {
		t.Fatal("seed functions need the ops pool")
	}
	if _, err := no.UpsertNE(ctx, &registry.NE{}, ""); err == nil {
		t.Fatal("UpsertNE needs the ops pool")
	}
}
