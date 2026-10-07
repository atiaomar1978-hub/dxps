// Package adapter implements the DxPS southbound adapter SPI (LLD 6.9) and the domain drivers for
// 5G SBA, IMS, NETCONF/RESTCONF, eSIM ES2+, BSS (TMF666/NPDB), access (TR-385/TR-369) and exposure (CAMARA/NEF).
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dxps/internal/contract"
	"dxps/internal/registry"
	"dxps/internal/saga"
)

const maxResponse = 1 << 20

// Call is one southbound HTTP request built by a driver.
type Call struct {
	Method      string
	Path        string
	Body        []byte
	ContentType string
	Headers     map[string]string
}

// Driver builds NE calls for one domain and optionally interprets 2xx business responses.
type Driver interface {
	Domain() string
	Supports(op string) bool
	Build(ctx context.Context, a *Adapter, ne *registry.NE, t *contract.NeTask) (*Call, error)
	Interpret(status int, body map[string]any) (contract.Outcome, string, string) // outcome, ne code, message
}

type Adapter struct {
	Registry *registry.Registry
	Client   *http.Client
	Drivers  map[string]Driver
	Now      func() time.Time

	mu     sync.Mutex
	tokens map[string]token
	stats  map[string]*NEStats
}

type token struct {
	value string
	exp   time.Time
}

type NEStats struct {
	NE      string `json:"ne"`
	Calls   int64  `json:"calls"`
	Errors  int64  `json:"errors"`
	LastUS  int64  `json:"lastUs"`
	TotalUS int64  `json:"totalUs"`
	Breaker string `json:"breaker"`
}

func New(reg *registry.Registry, client *http.Client) *Adapter {
	a := &Adapter{Registry: reg, Client: client, Drivers: map[string]Driver{}, tokens: map[string]token{}, stats: map[string]*NEStats{}}
	for _, d := range []Driver{sba{}, ims{}, netconf{}, esim{}, bss{}, access{}, exposure{}} {
		a.Drivers[d.Domain()] = d
	}
	return a
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func result(t *contract.NeTask, o contract.Outcome, status int, code, msg string) *contract.TaskResult {
	return &contract.TaskResult{Tenant: t.Tenant, TaskID: t.TaskID, OrderID: t.OrderID, Attempt: t.Attempt,
		Outcome: o, NEStatus: status, NECode: code, Message: msg}
}

func timeout(p contract.Priority) time.Duration {
	if p <= contract.P1 {
		return 3 * time.Second
	}
	return 10 * time.Second
}

// Execute runs one NE task; it never returns nil.
func (a *Adapter) Execute(ctx context.Context, t *contract.NeTask) *contract.TaskResult {
	d, ok := a.Drivers[t.Domain]
	if !ok || !d.Supports(t.Operation) {
		return result(t, contract.Failed, 0, "DXPS-1001", "operation not supported: "+t.Operation)
	}
	ne, ok := a.Registry.Get(t.NETenant, t.NEID)
	if !ok {
		return result(t, contract.Failed, 0, "DXPS-1003", "NE not found")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout(t.Priority))
	defer cancel()
	release, err := ne.Acquire(ctx, t.Tenant, t.Operation, t.Priority)
	switch {
	case errors.Is(err, registry.ErrNotEntitled):
		return result(t, contract.Failed, 0, "DXPS-1005", err.Error())
	case errors.Is(err, registry.ErrCircuitOpen):
		r := result(t, contract.Retryable, 0, "", err.Error())
		r.RetryAfter = 5 * time.Second
		return r
	case err != nil:
		return result(t, contract.Retryable, 0, "", "budget: "+err.Error())
	}
	defer release()
	call, err := d.Build(ctx, a, ne, t)
	if err != nil {
		var re retryableErr
		if errors.As(err, &re) {
			return result(t, contract.Retryable, 0, "", err.Error())
		}
		return result(t, contract.Failed, 0, "DXPS-1002", err.Error())
	}
	ep, err := ne.Endpoint()
	if err != nil {
		return result(t, contract.Failed, 0, "DXPS-1003", err.Error())
	}
	start := a.now()
	status, body, hdr, err := a.do(ctx, ep.BaseURI, call, t)
	lat := a.now().Sub(start)
	ne.Breaker.Record(err == nil && status < 500)
	a.record(ne, lat, err != nil || status >= 400)
	outcome := saga.Classify(status, err)
	r := result(t, outcome, status, "", "")
	r.LatencyUS = lat.Microseconds()
	if err != nil {
		r.Message = "transport: " + err.Error()
		return r
	}
	var parsed map[string]any
	if len(body) > 0 {
		if json.Unmarshal(body, &parsed) == nil {
			r.Response = body
		}
	}
	if ra := hdr.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s >= 0 && s < 600 {
			r.RetryAfter = time.Duration(s) * time.Second
		}
	}
	if outcome == contract.Succeeded {
		r.Outcome, r.NECode, r.Message = d.Interpret(status, parsed)
		return r
	}
	r.NECode, r.Message = problem(status, parsed)
	return r
}

// problem extracts an RFC 7807 / 3GPP ProblemDetails / TMF630 error description.
func problem(status int, m map[string]any) (string, string) {
	code := fmt.Sprintf("DXPS-3%03d", status%1000)
	msg := http.StatusText(status)
	for _, k := range []string{"cause", "code", "reason"} {
		if v, ok := m[k].(string); ok && v != "" {
			code = code + ":" + truncate(v, 40)
			break
		}
	}
	for _, k := range []string{"detail", "message", "title"} {
		if v, ok := m[k].(string); ok && v != "" {
			msg = truncate(v, 200)
			break
		}
	}
	return code, msg
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (a *Adapter) do(ctx context.Context, base string, c *Call, t *contract.NeTask) (int, []byte, http.Header, error) {
	var body io.Reader
	if c.Body != nil {
		body = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, strings.TrimRight(base, "/")+c.Path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if c.Body != nil {
		req.Header.Set("Content-Type", c.ContentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Idempotency-Key", t.IdempotencyKey)
	req.Header.Set("X-DxPS-Tenant", t.Tenant)
	req.Header.Set("X-DxPS-Task", t.TaskID)
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, b, resp.Header, err
}

func (a *Adapter) record(ne *registry.NE, lat time.Duration, failed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stats[ne.Code]
	if s == nil {
		s = &NEStats{NE: ne.Code}
		a.stats[ne.Code] = s
	}
	s.Calls++
	if failed {
		s.Errors++
	}
	s.LastUS = lat.Microseconds()
	s.TotalUS += s.LastUS
}

func (a *Adapter) Stats() []NEStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []NEStats
	for _, ne := range a.Registry.All() {
		s := NEStats{NE: ne.Code}
		if x := a.stats[ne.Code]; x != nil {
			s = *x
		}
		s.Breaker = ne.Breaker.State().String()
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------- helpers for drivers

type retryableErr struct{ error }

var safeSeg = regexp.MustCompile(`^[A-Za-z0-9@._:+/-]{1,128}$`)

// pathParam returns a URL-escaped, validated path segment from the task params.
func pathParam(t *contract.NeTask, name string) (string, error) {
	v, ok := t.Params[name]
	if !ok {
		return "", fmt.Errorf("missing path parameter %s", name)
	}
	s := fmt.Sprint(v)
	if !safeSeg.MatchString(s) || strings.Contains(s, "..") {
		return "", fmt.Errorf("invalid path parameter %s", name)
	}
	return url.PathEscape(s), nil
}

func expand(t *contract.NeTask, tpl string) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '{')
		if i < 0 {
			b.WriteString(tpl)
			return b.String(), nil
		}
		j := strings.IndexByte(tpl[i:], '}')
		if j < 0 {
			return "", errors.New("bad path template")
		}
		v, err := pathParam(t, tpl[i+1:i+j])
		if err != nil {
			return "", err
		}
		b.WriteString(tpl[:i])
		b.WriteString(v)
		tpl = tpl[i+j+1:]
	}
}

type route struct {
	method, path, ctype string
}

func buildRoute(routes map[string]route, t *contract.NeTask, headers map[string]string) (*Call, error) {
	r, ok := routes[t.Operation]
	if !ok {
		return nil, fmt.Errorf("unsupported operation %s", t.Operation)
	}
	p, err := expand(t, r.path)
	if err != nil {
		return nil, err
	}
	c := &Call{Method: r.method, Path: p, Headers: headers}
	if r.method != http.MethodDelete && r.method != http.MethodGet {
		c.Body, c.ContentType = t.Request, r.ctype
		if c.ContentType == "" {
			c.ContentType = "application/json"
		}
	}
	return c, nil
}

func supports(routes map[string]route, op string) bool { _, ok := routes[op]; return ok }

func ok2xx(int, map[string]any) (contract.Outcome, string, string) { return contract.Succeeded, "", "" }
