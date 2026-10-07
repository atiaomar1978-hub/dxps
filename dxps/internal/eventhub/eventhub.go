// Package eventhub delivers TMF688 notifications to tenant-registered listeners with HMAC-SHA256
// signatures, https-only allow-listed callbacks, no redirects and bounded retries.
package eventhub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"dxps/internal/contract"
	"dxps/internal/secretbox"
	"dxps/internal/store"
)

const SignatureHeader = "X-DxPS-Signature"

// Sign returns the signature header value: t=<unix>,v1=<hex(hmac_sha256(secret, "<unix>.<body>"))>.
func Sign(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(t + "."))
	m.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

var ErrSignature = errors.New("invalid webhook signature")

// Verify checks a signature header within tolerance (replay protection).
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts int64
	var sig string
	for _, p := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if ts == 0 || sig == "" {
		return ErrSignature
	}
	t := time.Unix(ts, 0)
	if now.Sub(t) > tolerance || t.Sub(now) > tolerance {
		return ErrSignature
	}
	want := Sign(secret, t, body)
	_, wantSig, _ := strings.Cut(want, ",v1=")
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return ErrSignature
	}
	return nil
}

// Matches evaluates a TMF688 listener query (k=v pairs joined by &, supported keys: eventType, state).
func Matches(query string, ev *contract.OrderEvent) bool {
	if query == "" {
		return true
	}
	vals, err := url.ParseQuery(query)
	if err != nil {
		return false
	}
	for k, vs := range vals {
		var have string
		switch k {
		case "eventType":
			have = ev.EventType
		case "state", "event.serviceOrder.state":
			have = ev.Event.ServiceOrder.State
		default:
			return false
		}
		ok := false
		for _, v := range vs {
			for _, x := range strings.Split(v, ",") {
				if x == have {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

type Delivery struct {
	Tenant   string    `json:"tenant"`
	Hub      string    `json:"hub"`
	Callback string    `json:"callback"`
	Order    string    `json:"order"`
	State    string    `json:"state"`
	Status   int       `json:"status"`
	Attempts int       `json:"attempts"`
	OK       bool      `json:"ok"`
	At       time.Time `json:"at"`
}

type Hub struct {
	Store    *store.Store
	Box      *secretbox.Box
	Client   *http.Client
	Allow    []string
	Log      *slog.Logger
	Backoff  time.Duration
	Attempts int

	mu     sync.Mutex
	recent []Delivery
	ok     int64
	failed int64
}

// NewClient returns an HTTP client that never follows redirects.
func NewClient(base *http.Client) *http.Client {
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	return &c
}

func (h *Hub) allowed(cb string) bool {
	u, err := url.Parse(cb)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	for _, a := range h.Allow {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}

// Handle delivers one event to all matching listeners of its tenant.
func (h *Hub) Handle(ctx context.Context, ev *contract.OrderEvent, headerTenant, key string) error {
	if err := contract.CheckTenant(headerTenant, key, ev.Tenant); err != nil {
		return err
	}
	var hubs []store.Hub
	if err := h.Store.InTenant(ctx, ev.Tenant, func(tx pgx.Tx) error {
		var err error
		hubs, err = store.ListHubs(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	body, _ := json.Marshal(ev)
	for _, s := range hubs {
		if s.Tenant != ev.Tenant || !Matches(s.Query, ev) {
			continue
		}
		d := Delivery{Tenant: ev.Tenant, Hub: s.ID, Callback: s.Callback, Order: ev.Event.ServiceOrder.ID,
			State: ev.Event.ServiceOrder.State, At: time.Now()}
		secret, err := h.Box.Open(ev.Tenant, s.SecretRef)
		if err != nil || !h.allowed(s.Callback) {
			d.Status = -1
			h.note(d)
			continue
		}
		d.Status, d.Attempts, d.OK = h.deliver(ctx, s.Callback, secret, ev, body)
		h.note(d)
	}
	return nil
}

func (h *Hub) deliver(ctx context.Context, cb string, secret []byte, ev *contract.OrderEvent, body []byte) (int, int, bool) {
	attempts := max(h.Attempts, 1)
	back := h.Backoff
	if back == 0 {
		back = 200 * time.Millisecond
	}
	status := 0
	for i := 1; i <= attempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cb, bytes.NewReader(body))
		if err != nil {
			return 0, i, false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-DxPS-Event-Id", ev.EventID)
		req.Header.Set("X-DxPS-Tenant", ev.Tenant)
		req.Header.Set(SignatureHeader, Sign(secret, time.Now(), body))
		resp, err := h.Client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			status = resp.StatusCode
			if status >= 200 && status < 300 {
				return status, i, true
			}
			if status < 500 && status != 408 && status != 429 {
				return status, i, false
			}
		}
		select {
		case <-time.After(back << (i - 1)):
		case <-ctx.Done():
			return status, i, false
		}
	}
	return status, attempts, false
}

func (h *Hub) note(d Delivery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d.OK {
		h.ok++
	} else {
		h.failed++
		if h.Log != nil {
			h.Log.Warn("webhook delivery failed", "tenant", d.Tenant, "hub", d.Hub, "status", d.Status)
		}
	}
	h.recent = append(h.recent, d)
	if len(h.recent) > 30 {
		h.recent = h.recent[len(h.recent)-30:]
	}
}

type Stats struct {
	Delivered int64      `json:"delivered"`
	Failed    int64      `json:"failed"`
	Recent    []Delivery `json:"recent"`
}

func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Stats{Delivered: h.ok, Failed: h.failed, Recent: append([]Delivery(nil), h.recent...)}
}

// Decode parses an event record and restores the tenant from the header.
func Decode(value []byte, headerTenant string) (*contract.OrderEvent, error) {
	var ev contract.OrderEvent
	if err := json.Unmarshal(value, &ev); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	ev.Tenant = headerTenant
	return &ev, nil
}
