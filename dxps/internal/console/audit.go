package console

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditEntry is one console action (who did what); DxPS's own audit_event rows are read from the database.
type AuditEntry struct {
	At     time.Time      `json:"at"`
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Object string         `json:"object,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
	Source string         `json:"source"`
}

// AuditLog appends to a JSON-lines file (mode 0600).
type AuditLog struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
}

func NewAuditLog(path string) *AuditLog { return &AuditLog{path: path, now: time.Now} }

func (a *AuditLog) Add(actor, action, object string, detail map[string]any) error {
	e := AuditEntry{At: a.now().UTC(), Actor: actor, Action: action, Object: object, Detail: detail, Source: "console"}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- operator runtime path
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Recent returns up to limit entries, newest first, optionally filtered by a text match.
func (a *AuditLog) Recent(limit int, text string) ([]AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.path) // #nosec G304 -- operator runtime path
	if os.IsNotExist(err) {
		return []AuditEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if text != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(text)) {
			continue
		}
		var e AuditEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			all = append(all, e)
		}
	}
	out := make([]AuditEntry, 0, min(limit, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, sc.Err()
}
