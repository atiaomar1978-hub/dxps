package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type NewOrder struct {
	Tenant, ID, ExternalID, Channel, Category string
	Priority                                  int
	TxMode                                    string
	IdempotencyKey                            string
	RequestHash                               []byte
	Items                                     json.RawMessage
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateOrder inserts the order idempotently. created=false means the same Idempotency-Key was
// replayed with the same body; a different body (or a duplicate externalId) yields ErrConflict.
func CreateOrder(ctx context.Context, tx pgx.Tx, o NewOrder) (created bool, err error) {
	ct, err := tx.Exec(ctx, `INSERT INTO service_order (tenant, id, external_id, channel, category, state, priority, tx_mode,
			idempotency_key, request_hash, items, requested_at)
		VALUES ($1,$2,$3,$4,$5,'acknowledged',$6,$7,$8,$9,$10,now())
		ON CONFLICT (tenant, idempotency_key) DO NOTHING`,
		o.Tenant, o.ID, nullable(o.ExternalID), o.Channel, nullable(o.Category), o.Priority, o.TxMode,
		o.IdempotencyKey, o.RequestHash, o.Items)
	if err != nil {
		if isUnique(err) {
			return false, ErrConflict
		}
		return false, err
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}
	var id string
	var h []byte
	if err := tx.QueryRow(ctx, `SELECT id::text, request_hash FROM service_order WHERE tenant=$1 AND idempotency_key=$2`,
		o.Tenant, o.IdempotencyKey).Scan(&id, &h); err != nil {
		return false, err
	}
	if id != o.ID || !bytes.Equal(h, o.RequestHash) {
		return false, ErrConflict
	}
	return false, nil
}

type ItemState struct {
	ItemID    string `json:"id"`
	State     string `json:"state"`
	Spec      string `json:"spec"`
	ErrorCode string `json:"errorCode,omitempty"`
	Error     string `json:"error,omitempty"`
}

type OrderView struct {
	Tenant      string
	ID          string
	ExternalID  string
	Channel     string
	Category    string
	State       string
	Priority    int
	TxMode      string
	Items       json.RawMessage
	RequestedAt time.Time
	CompletedAt *time.Time
	ErrorCode   string
	Error       string
	ItemStates  []ItemState
}

const orderCols = `tenant, id::text, coalesce(external_id,''), channel, coalesce(category,''), state, priority, tx_mode,
	items, requested_at, completed_at, coalesce(error_code,''), coalesce(error,'')`

func scanOrder(r pgx.Row) (*OrderView, error) {
	o := &OrderView{}
	err := r.Scan(&o.Tenant, &o.ID, &o.ExternalID, &o.Channel, &o.Category, &o.State, &o.Priority, &o.TxMode,
		&o.Items, &o.RequestedAt, &o.CompletedAt, &o.ErrorCode, &o.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

func itemStates(ctx context.Context, tx pgx.Tx, o *OrderView) error {
	rows, err := tx.Query(ctx, `SELECT order_item_id, state, spec_code, coalesce(error_code,''), coalesce(error,'')
		FROM business_command WHERE tenant = current_tenant() AND order_id = $1 ORDER BY order_item_id`, o.ID)
	if err != nil {
		return err
	}
	o.ItemStates, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ItemState, error) {
		var s ItemState
		return s, r.Scan(&s.ItemID, &s.State, &s.Spec, &s.ErrorCode, &s.Error)
	})
	return err
}

// GetOrder reads an order of the transaction's tenant (RLS makes other tenants' orders invisible).
func GetOrder(ctx context.Context, tx pgx.Tx, id string) (*OrderView, error) {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderCols+` FROM service_order WHERE tenant = current_tenant() AND id = $1`, id))
	if err != nil {
		return nil, err
	}
	return o, itemStates(ctx, tx, o)
}

func ListOrders(ctx context.Context, tx pgx.Tx, state string, limit, offset int) ([]*OrderView, error) {
	rows, err := tx.Query(ctx, `SELECT `+orderCols+` FROM service_order
		WHERE tenant = current_tenant() AND ($1 = '' OR state = $1)
		ORDER BY requested_at DESC, id DESC LIMIT $2 OFFSET $3`, state, limit, offset)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*OrderView, error) { return scanOrder(r) })
	if err != nil {
		return nil, err
	}
	for _, o := range out {
		if err := itemStates(ctx, tx, o); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CancelOrder cancels an order whose business commands have not started on any NE.
func CancelOrder(ctx context.Context, tx pgx.Tx, id string) (*OrderView, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM service_order WHERE tenant = current_tenant() AND id = $1 FOR UPDATE`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM business_command WHERE tenant = current_tenant() AND order_id = $1
		AND state NOT IN ('pending','cancelled')`, id).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 || state != "acknowledged" && state != "held" && state != "pending" {
		return nil, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE business_command SET state='cancelled', updated_at=now()
		WHERE tenant = current_tenant() AND order_id=$1`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_order SET state='cancelled', completed_at=now(), updated_at=now(), version=version+1
		WHERE tenant = current_tenant() AND id=$1`, id); err != nil {
		return nil, err
	}
	return GetOrder(ctx, tx, id)
}

// ---------------------------------------------------------------- TMF688 hub subscriptions

type Hub struct {
	Tenant    string
	ID        string
	Callback  string
	Query     string
	SecretRef string
	Status    string
}

func InsertHub(ctx context.Context, tx pgx.Tx, h *Hub) error {
	return tx.QueryRow(ctx, `INSERT INTO hub_subscription (tenant, callback, query, secret_ref, status)
		VALUES ($1,$2,$3,$4,'ACTIVE') RETURNING id::text`, h.Tenant, h.Callback, nullable(h.Query), h.SecretRef).Scan(&h.ID)
}

func DeleteHub(ctx context.Context, tx pgx.Tx, id string) error {
	ct, err := tx.Exec(ctx, `DELETE FROM hub_subscription WHERE tenant = current_tenant() AND id = $1`, id)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func ListHubs(ctx context.Context, tx pgx.Tx) ([]Hub, error) {
	rows, err := tx.Query(ctx, `SELECT tenant, id::text, callback, coalesce(query,''), secret_ref, status
		FROM hub_subscription WHERE tenant = current_tenant() AND status = 'ACTIVE' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hub, error) {
		var h Hub
		return h, r.Scan(&h.Tenant, &h.ID, &h.Callback, &h.Query, &h.SecretRef, &h.Status)
	})
}
