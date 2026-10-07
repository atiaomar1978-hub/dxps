// Package store is the PostgreSQL access layer (pgx v5). All tenant data access goes through
// InTenant, which sets the transaction-local app.tenant used by row-level security.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"dxps/internal/tenant"
)

//go:embed migrations/*.sql
var migrations embed.FS

const OutboxShards = 16

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	App *pgxpool.Pool // dxps_app: RLS enforced
	Ops *pgxpool.Pool // dxps_ops: BYPASSRLS (relay, registry loader, dashboard), may be nil
}

func newPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg config: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["search_path"] = "dxps"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func Open(ctx context.Context, appDSN, opsDSN string) (*Store, error) {
	s := &Store{}
	var err error
	if appDSN != "" {
		if s.App, err = newPool(ctx, appDSN, 32); err != nil {
			return nil, err
		}
	}
	if opsDSN != "" {
		if s.Ops, err = newPool(ctx, opsDSN, 8); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() {
	if s.App != nil {
		s.App.Close()
	}
	if s.Ops != nil {
		s.Ops.Close()
	}
}

// InTenant runs fn in a READ COMMITTED transaction scoped to tenantID for RLS.
func (s *Store) InTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if !tenant.Valid(tenantID) {
		return tenant.ErrInvalidID
	}
	return pgx.BeginTxFunc(ctx, s.App, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant', $1, true)", tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Retryable reports transient PostgreSQL errors (serialization failure, deadlock).
func Retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---------------------------------------------------------------- migrations

func Migrate(ctx context.Context, ownerDSN string) ([]string, error) {
	conn, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(7731)"); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(7731)")
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(files)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('dxps.schema_migration') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	var applied []string
	for _, f := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		if exists {
			var n int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM dxps.schema_migration WHERE tenant='GLOBAL' AND version=$1", v).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				continue
			}
		}
		sql, _ := migrations.ReadFile(f)
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("%s: %w", v, err)
			}
			_, err := tx.Exec(ctx, "INSERT INTO dxps.schema_migration (version) VALUES ($1)", v)
			return err
		})
		if err != nil {
			return applied, err
		}
		exists = true
		applied = append(applied, v)
	}
	return applied, nil
}

// ---------------------------------------------------------------- outbox

type OutboxRow struct {
	Tenant  string
	ID      string
	Topic   string
	Key     string
	Payload []byte
	Headers map[string]string
}

func Shard(key string) int16 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int16(h.Sum32() % OutboxShards)
}

// InsertOutbox writes rows in the current tenant transaction and fills their ids.
func InsertOutbox(ctx context.Context, tx pgx.Tx, rows []*OutboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(`INSERT INTO outbox (tenant, shard, topic, key, payload, headers) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
			r.Tenant, Shard(r.Key), r.Topic, []byte(r.Key), r.Payload, r.Headers)
	}
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	for _, r := range rows {
		if err := br.QueryRow().Scan(&r.ID); err != nil {
			return err
		}
	}
	return nil
}

// MarkPublished marks outbox rows as published (ops role, after a successful produce).
func (s *Store) MarkPublished(ctx context.Context, rows []*OutboxRow) error {
	if len(rows) == 0 || s.Ops == nil {
		return nil
	}
	ten := make([]string, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		ten[i], ids[i] = r.Tenant, r.ID
	}
	_, err := s.Ops.Exec(ctx, `UPDATE outbox o SET published_at = now()
		FROM unnest($1::text[], $2::uuid[]) AS u(tenant, id)
		WHERE o.tenant = u.tenant AND o.id = u.id AND o.published_at IS NULL`, ten, ids)
	return err
}

// RelayBatch locks up to limit unpublished rows older than minAge, calls publish, then marks them.
func (s *Store) RelayBatch(ctx context.Context, minAge time.Duration, limit int, publish func([]*OutboxRow) error) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.Ops, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant, id::text, topic, convert_from(key, 'UTF8'), payload, headers FROM outbox
			WHERE published_at IS NULL AND created_at < now() - make_interval(secs => $1)
			ORDER BY shard, id LIMIT $2 FOR UPDATE SKIP LOCKED`, minAge.Seconds(), limit)
		if err != nil {
			return err
		}
		out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*OutboxRow, error) {
			o := &OutboxRow{}
			return o, r.Scan(&o.Tenant, &o.ID, &o.Topic, &o.Key, &o.Payload, &o.Headers)
		})
		if err != nil || len(out) == 0 {
			return err
		}
		if err := publish(out); err != nil {
			return err
		}
		ten := make([]string, len(out))
		ids := make([]string, len(out))
		for i, r := range out {
			ten[i], ids[i] = r.Tenant, r.ID
		}
		n = len(out)
		_, err = tx.Exec(ctx, `UPDATE outbox o SET published_at = now() FROM unnest($1::text[], $2::uuid[]) AS u(tenant, id)
			WHERE o.tenant = u.tenant AND o.id = u.id`, ten, ids)
		return err
	})
	return n, err
}

// ---------------------------------------------------------------- audit

type Audit struct {
	Actor      string
	ActingFor  bool
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
}

func InsertAudit(ctx context.Context, tx pgx.Tx, tenantID string, a Audit) error {
	var oid any
	if a.ObjectID != "" {
		oid = a.ObjectID
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_event (tenant, actor, acting_for_tenant, action, object_type, object_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenantID, a.Actor, a.ActingFor, a.Action, a.ObjectType, oid, a.Detail)
	return err
}
