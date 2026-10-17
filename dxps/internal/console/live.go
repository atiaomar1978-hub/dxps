package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/auth"
	"dxps/internal/bus"
	"dxps/internal/contract"
)

// Live is everything the console reads from or sends to running DxPS processes.
type Live interface {
	Health(ctx context.Context) map[string]bool
	ServiceStats(ctx context.Context) map[string]json.RawMessage
	Kafka(ctx context.Context) (*KafkaView, error)
	DLQ(ctx context.Context, limit int) ([]DLQRecord, error)
	Redrive(ctx context.Context) (replayed, skipped int, err error)
	Sims(ctx context.Context) (stats, exchanges json.RawMessage, err error)
	Fault(ctx context.Context, f FaultReq) error
	Mosaic(ctx context.Context, method, path string, body any) (int, json.RawMessage, error)
	Gateway(ctx context.Context, tenantID, subject, method, path string, body []byte, idem string) (int, json.RawMessage, error)
	Tests() json.RawMessage
}

type FaultReq struct {
	Sim       string  `json:"sim"`
	LatencyMs int     `json:"latencyMs"`
	ErrorRate float64 `json:"errorRate"`
}

type KafkaView struct {
	Topics []bus.TopicStat `json:"topics"`
	Lags   []bus.GroupLag  `json:"lags"`
}

type DLQRecord struct {
	Partition int32             `json:"partition"`
	Offset    int64             `json:"offset"`
	At        time.Time         `json:"at"`
	Key       string            `json:"key"`
	Source    string            `json:"sourceTopic"`
	Error     string            `json:"error"`
	Headers   map[string]string `json:"headers"`
	Payload   json.RawMessage   `json:"payload,omitempty"`
	Text      string            `json:"text,omitempty"`
}

// Endpoints are the loopback URLs of the DxPS processes.
type Endpoints struct {
	Stats       map[string]string // service -> http://127.0.0.1:92xx/stats
	NetsimAdmin string
	NetsimToken string
	Mosaic      string // http://127.0.0.1:8088
	GatewayURL  string // https://localhost:8443
	Results     string // tests.json
	Kafka       []string
}

type liveImpl struct {
	ep      Endpoints
	local   *http.Client
	gw      *http.Client
	signer  *auth.Signer
	admin   *bus.Admin
	mosaicM sync.Mutex
	csrf    string
	cookie  string
}

func NewLive(ep Endpoints, gw *http.Client, signer *auth.Signer) (Live, error) {
	adm, err := bus.NewAdmin(ep.Kafka)
	if err != nil {
		return nil, err
	}
	return &liveImpl{ep: ep, local: &http.Client{Timeout: 3 * time.Second}, gw: gw, signer: signer, admin: adm}, nil
}

func (l *liveImpl) get(ctx context.Context, url string, hdr map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := l.local.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || !json.Valid(b) {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return b, nil
}

func (l *liveImpl) Health(ctx context.Context) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	set := func(k string, ok bool) { mu.Lock(); out[k] = ok; mu.Unlock() }
	for name, url := range l.ep.Stats {
		wg.Go(func() { _, err := l.get(ctx, url, nil); set(name, err == nil) })
	}
	wg.Go(func() {
		_, err := l.get(ctx, l.ep.NetsimAdmin+"/stats", map[string]string{"X-Netsim-Admin": l.ep.NetsimToken})
		set("netsim", err == nil)
	})
	wg.Go(func() { _, err := l.get(ctx, l.ep.Mosaic+"/api/session", nil); set("mosaic", err == nil) })
	wg.Go(func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.ep.GatewayURL+"/healthz", nil)
		resp, err := l.gw.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		set("gateway", err == nil && resp.StatusCode == http.StatusOK)
	})
	wg.Go(func() { _, err := l.admin.TopicEnds(ctx); set("kafka", err == nil) })
	wg.Wait()
	return out
}

func (l *liveImpl) ServiceStats(ctx context.Context) map[string]json.RawMessage {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out := map[string]json.RawMessage{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, url := range l.ep.Stats {
		wg.Go(func() {
			if b, err := l.get(ctx, url, nil); err == nil {
				mu.Lock()
				out[name] = b
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

func (l *liveImpl) Kafka(ctx context.Context) (*KafkaView, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	t, err := l.admin.TopicEnds(ctx)
	if err != nil {
		return nil, err
	}
	g, err := l.admin.Lags(ctx)
	if err != nil {
		return nil, err
	}
	return &KafkaView{Topics: t, Lags: g}, nil
}

// DLQ reads the last records of the dead-letter topic without joining a consumer group.
func (l *liveImpl) DLQ(ctx context.Context, limit int) ([]DLQRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, err := kgo.NewClient(kgo.SeedBrokers(l.ep.Kafka...), kgo.ClientID("dxps-console"), kgo.DialTimeout(3*time.Second))
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	starts, err := adm.ListStartOffsets(ctx, contract.TopicDLQ)
	if err != nil {
		return nil, err
	}
	ends, err := adm.ListEndOffsets(ctx, contract.TopicDLQ)
	if err != nil {
		return nil, err
	}
	from := map[int32]kgo.Offset{}
	want := map[int32]int64{}
	ends.Each(func(o kadm.ListedOffset) {
		if o.Err != nil {
			return
		}
		st := int64(0)
		if s, ok := starts.Lookup(o.Topic, o.Partition); ok && s.Err == nil {
			st = s.Offset
		}
		if o.Offset > st {
			from[o.Partition] = kgo.NewOffset().At(max(st, o.Offset-int64(limit)))
			want[o.Partition] = o.Offset
		}
	})
	if len(from) == 0 {
		return []DLQRecord{}, nil
	}
	rc, err := kgo.NewClient(kgo.SeedBrokers(l.ep.Kafka...), kgo.ClientID("dxps-console"),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{contract.TopicDLQ: from}))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var out []DLQRecord
	for len(want) > 0 && ctx.Err() == nil {
		fs := rc.PollFetches(ctx)
		if fs.IsClientClosed() || ctx.Err() != nil {
			break
		}
		fs.EachRecord(func(r *kgo.Record) {
			d := DLQRecord{Partition: r.Partition, Offset: r.Offset, At: r.Timestamp, Key: string(r.Key), Headers: map[string]string{}}
			for _, h := range r.Headers {
				switch h.Key {
				case "source_topic":
					d.Source = string(h.Value)
				case "error":
					d.Error = string(h.Value)
				default:
					d.Headers[h.Key] = string(h.Value)
				}
			}
			if json.Valid(r.Value) {
				d.Payload = append(json.RawMessage(nil), r.Value...)
			} else {
				d.Text = string(r.Value[:min(len(r.Value), 2048)])
			}
			out = append(out, d)
			if r.Offset+1 >= want[r.Partition] {
				delete(want, r.Partition)
			}
		})
	}
	// newest first, at most limit
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Redrive replays dead-lettered business-command, task and task-result records to their source topic,
// committing the "dxps-dlq-redrive" group (same rules as dxpsctl redrive).
func (l *liveImpl) Redrive(ctx context.Context) (int, int, error) {
	allowed := map[string]bool{contract.TopicTaskResult: true}
	for _, t := range bus.Topics() {
		if strings.HasPrefix(t.Name, "dxps.bc.") || strings.HasPrefix(t.Name, "dxps.task.") {
			allowed[t.Name] = true
		}
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(l.ep.Kafka...), kgo.ConsumerGroup("dxps-dlq-redrive"), kgo.ConsumeTopics(contract.TopicDLQ),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return 0, 0, err
	}
	defer cl.Close()
	p, err := bus.NewProducer(l.ep.Kafka, "dxps-console-redrive")
	if err != nil {
		return 0, 0, err
	}
	defer p.Close()
	n, skipped := 0, 0
	for i := 0; i < 100; i++ {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		fetches := cl.PollFetches(c)
		cancel()
		if fetches.Empty() {
			break
		}
		var recs []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			src := bus.Header(r, "source_topic")
			if !allowed[src] {
				skipped++
				return
			}
			hs := map[string]string{}
			for _, h := range r.Headers {
				if h.Key != "error" && h.Key != "source_topic" {
					hs[h.Key] = string(h.Value)
				}
			}
			recs = append(recs, bus.Record(src, string(r.Key), r.Value, hs))
		})
		c2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
		err := p.Publish(c2, recs...)
		if err == nil {
			err = cl.CommitUncommittedOffsets(c2)
		}
		cancel2()
		if err != nil {
			return n, skipped, err
		}
		n += len(recs)
	}
	return n, skipped, nil
}

func (l *liveImpl) Sims(ctx context.Context) (json.RawMessage, json.RawMessage, error) {
	h := map[string]string{"X-Netsim-Admin": l.ep.NetsimToken}
	st, err := l.get(ctx, l.ep.NetsimAdmin+"/stats", h)
	if err != nil {
		return nil, nil, err
	}
	ex, err := l.get(ctx, l.ep.NetsimAdmin+"/exchanges", h)
	if err != nil {
		return nil, nil, err
	}
	return st, ex, nil
}

func (l *liveImpl) Fault(ctx context.Context, f FaultReq) error {
	b, _ := json.Marshal(f)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.ep.NetsimAdmin+"/faults", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Netsim-Admin", l.ep.NetsimToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.local.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("netsim: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Mosaic calls the Mosaic dashboard API the way its own page does (CSRF cookie + header, same origin).
func (l *liveImpl) Mosaic(ctx context.Context, method, p string, body any) (int, json.RawMessage, error) {
	l.mosaicM.Lock()
	defer l.mosaicM.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if method != http.MethodGet && l.csrf == "" {
			if err := l.mosaicSession(ctx); err != nil {
				return 0, nil, err
			}
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, l.ep.Mosaic+p, rd)
		if err != nil {
			return 0, nil, err
		}
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", l.ep.Mosaic)
			req.Header.Set("X-CSRF-Token", l.csrf)
			req.Header.Set("Cookie", l.cookie)
		}
		resp, err := l.local.Do(req)
		if err != nil {
			return 0, nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden && attempt == 0 && method != http.MethodGet {
			l.csrf = ""
			continue
		}
		if !json.Valid(b) {
			b, _ = json.Marshal(map[string]string{"error": strings.TrimSpace(string(b))})
		}
		return resp.StatusCode, b, nil
	}
	return 0, nil, errors.New("mosaic: csrf rejected")
}

func (l *liveImpl) mosaicSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.ep.Mosaic+"/api/session", nil)
	if err != nil {
		return err
	}
	resp, err := l.local.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var s struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return err
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dxps_csrf" {
			l.cookie = c.Name + "=" + c.Value
		}
	}
	l.csrf = s.CSRF
	return nil
}

// Gateway calls the TMF641 API as tenantID with a short-lived token (subject = console user).
func (l *liveImpl) Gateway(ctx context.Context, tenantID, subject, method, p string, body []byte, idem string) (int, json.RawMessage, error) {
	tok, err := l.signer.Issue(subject, tenantID, []string{auth.ScopeOrderWrite, auth.ScopeOrderRead}, 2*time.Minute)
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.ep.GatewayURL+"/tmf-api/serviceOrdering/v5"+p, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := l.gw.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if !json.Valid(b) {
		b, _ = json.Marshal(map[string]string{"error": strings.TrimSpace(string(b))})
	}
	return resp.StatusCode, b, nil
}

func (l *liveImpl) Tests() json.RawMessage {
	b, err := os.ReadFile(l.ep.Results) // #nosec G304 -- runtime results path
	if err != nil || !json.Valid(b) {
		return nil
	}
	return b
}
