// Package contract defines the dxps.v1 Kafka message contracts (BusinessCommand, NeTask, TaskResult,
// OrderEvent), topic names, record keys and headers. Payloads are JSON-encoded in this build.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dxps/internal/tenant"
)

type Priority int

const (
	P0 Priority = iota // emergency / lawful / fraud barring
	P1                 // interactive (retail, app, MVNO self-care)
	P2                 // standard back-office
	P3                 // bulk / migration
)

func (p Priority) Valid() bool    { return p >= P0 && p <= P3 }
func (p Priority) String() string { return fmt.Sprintf("p%d", int(p)) }

func ParsePriority(s string) (Priority, error) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "p")
	if len(s) == 1 && s[0] >= '0' && s[0] <= '3' {
		return Priority(s[0] - '0'), nil
	}
	return 0, fmt.Errorf("invalid priority %q", s)
}

type TxMode string

const (
	Atomic         TxMode = "ATOMIC"
	PartialAllowed TxMode = "PARTIAL_ALLOWED"
	BestEffort     TxMode = "BEST_EFFORT"
)

func (m TxMode) Valid() bool { return m == Atomic || m == PartialAllowed || m == BestEffort }

// Domains served by adapters.
var Domains = []string{"sba", "ims", "netconf", "esim", "bss", "access", "exposure"}

// ---------------------------------------------------------------- topics

func BCTopic(p Priority) string                  { return "dxps.bc." + p.String() }
func TaskTopic(domain string, p Priority) string { return "dxps.task." + domain + "." + p.String() }

const (
	TopicTaskResult  = "dxps.task.result"
	TopicRetry5s     = "dxps.retry.5s"
	TopicRetry30s    = "dxps.retry.30s"
	TopicRetry5m     = "dxps.retry.5m"
	TopicDLQ         = "dxps.dlq"
	TopicStateOrder  = "dxps.state.order"
	TopicStateNE     = "dxps.state.ne-health"
	TopicStateTenant = "dxps.state.tenant"
	TopicEvent       = "dxps.event.tmf688"
)

var RetryTopics = []string{TopicRetry5s, TopicRetry30s, TopicRetry5m}

// RetryTopicFor returns the retry-ladder topic for a delay.
func RetryTopicFor(d time.Duration) string {
	switch {
	case d <= 5*time.Second:
		return TopicRetry5s
	case d <= 30*time.Second:
		return TopicRetry30s
	default:
		return TopicRetry5m
	}
}

// ---------------------------------------------------------------- keys & headers

const (
	HdrTenant      = "tenant"
	HdrMessageID   = "message_id"
	HdrPriority    = "priority"
	HdrTraceParent = "traceparent"
	HdrNotBefore   = "not_before"
	HdrTarget      = "target_topic"
	HdrProducer    = "producer_app"
	HdrSchema      = "schema_id"
)

func Key(parts ...string) string { return strings.Join(parts, "|") }

// TenantOfKey returns the tenant prefix of a record key.
func TenantOfKey(k string) string {
	t, _, _ := strings.Cut(k, "|")
	return t
}

// ---------------------------------------------------------------- messages

type BusinessCommand struct {
	Tenant          string         `json:"tenant"`
	MessageID       string         `json:"message_id"`
	OrderID         string         `json:"order_id"`
	OrderItemID     string         `json:"order_item_id"`
	BCID            string         `json:"bc_id"`
	EntityKey       string         `json:"entity_key"`
	EntitySeq       int64          `json:"entity_seq"`
	ExtraEntityKeys []string       `json:"extra_entity_keys,omitempty"`
	CommandSpec     string         `json:"command_spec"`
	SpecVersion     string         `json:"spec_version"`
	Action          string         `json:"action"`
	Priority        Priority       `json:"priority"`
	TxMode          TxMode         `json:"tx_mode"`
	Params          map[string]any `json:"params"`
	DependsOnItems  []string       `json:"depends_on_items,omitempty"`
	RequestedStart  *time.Time     `json:"requested_start,omitempty"`
	Deadline        *time.Time     `json:"deadline,omitempty"`
	Channel         string         `json:"channel"`
}

func (b *BusinessCommand) EntityKeys() []string {
	return append([]string{b.EntityKey}, b.ExtraEntityKeys...)
}

type NeTask struct {
	Tenant          string          `json:"tenant"`
	TaskID          string          `json:"task_id"`
	OrderID         string          `json:"order_id"`
	NETenant        string          `json:"ne_tenant"`
	NEID            string          `json:"ne_id"`
	NECode          string          `json:"ne_code"`
	Domain          string          `json:"domain"`
	EntityKey       string          `json:"entity_key"`
	Operation       string          `json:"operation"`
	Request         json.RawMessage `json:"request"`
	Params          map[string]any  `json:"params,omitempty"`
	SubscriberGroup string          `json:"subscriber_group,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Attempt         int             `json:"attempt"`
	Priority        Priority        `json:"priority"`
	NotBefore       *time.Time      `json:"not_before,omitempty"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	Compensation    bool            `json:"compensation"`
}

type Outcome string

const (
	Succeeded Outcome = "SUCCEEDED"
	Retryable Outcome = "RETRYABLE"
	Failed    Outcome = "FAILED"
)

type TaskResult struct {
	Tenant     string          `json:"tenant"`
	TaskID     string          `json:"task_id"`
	OrderID    string          `json:"order_id"`
	Attempt    int             `json:"attempt"`
	Outcome    Outcome         `json:"outcome"`
	NEStatus   int             `json:"ne_status"`
	NECode     string          `json:"ne_code,omitempty"`
	Message    string          `json:"message,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	LatencyUS  int64           `json:"latency_us"`
	RetryAfter time.Duration   `json:"retry_after,omitempty"`
}

// OrderEvent is the TMF688 ServiceOrderStateChangeEvent emitted on order state changes.
type OrderEvent struct {
	EventID   string    `json:"eventId"`
	EventTime time.Time `json:"eventTime"`
	EventType string    `json:"eventType"`
	Tenant    string    `json:"-"`
	Event     struct {
		ServiceOrder OrderRef `json:"serviceOrder"`
	} `json:"event"`
}

type OrderRef struct {
	ID             string     `json:"id"`
	ExternalID     string     `json:"externalId,omitempty"`
	State          string     `json:"state"`
	CompletionDate *time.Time `json:"completionDate,omitempty"`
}

// ---------------------------------------------------------------- validation

var (
	ErrTenantMismatch = errors.New("DXPS-1004: tenant header/payload mismatch")
	ErrInvalid        = errors.New("invalid message")
)

// CheckTenant enforces that the record header, key prefix and payload tenant agree.
func CheckTenant(header, key, payload string) error {
	if !tenant.Valid(payload) || header != payload || TenantOfKey(key) != payload {
		return ErrTenantMismatch
	}
	return nil
}

func (b *BusinessCommand) Validate() error {
	switch {
	case !tenant.Valid(b.Tenant), b.MessageID == "", b.OrderID == "", b.BCID == "",
		b.EntityKey == "", b.CommandSpec == "", !b.Priority.Valid(), !b.TxMode.Valid():
		return ErrInvalid
	}
	return nil
}
