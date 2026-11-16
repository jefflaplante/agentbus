package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
)

// AuditRecord is one line of the audit file. Bodies are never recorded:
// they may hold prompts, private data or ciphertext. BodySHA256 lets you
// match a record to a body you have from elsewhere.
type AuditRecord struct {
	TS            time.Time    `json:"ts"`
	Action        string       `json:"action"` // accept, reject, duplicate, connect, disconnect, refused, restore
	Agent         string       `json:"agent,omitempty"`
	Remote        string       `json:"remote,omitempty"`
	Topic         string       `json:"topic,omitempty"`
	ID            string       `json:"id,omitempty"`
	Type          a2a.Type     `json:"type,omitempty"`
	From          string       `json:"from,omitempty"`
	To            string       `json:"to,omitempty"`
	CorrelationID string       `json:"correlation_id,omitempty"`
	Capability    string       `json:"capability,omitempty"`
	Priority      a2a.Priority `json:"priority,omitempty"`
	Event         string       `json:"event,omitempty"`
	Code          string       `json:"code,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	Bytes         int          `json:"bytes,omitempty"`
	BodySHA256    string       `json:"body_sha256,omitempty"`
}

// envelopeRecord fills the envelope fields of a record.
func envelopeRecord(action, topic string, e *a2a.Envelope, size int) AuditRecord {
	r := AuditRecord{
		Action: action, Agent: e.From, Topic: topic, ID: e.ID, Type: e.Type, From: e.From, To: e.To,
		CorrelationID: e.CorrelationID, Capability: e.Capability, Priority: e.Priority, Event: e.Event, Bytes: size,
	}
	if len(e.Body) > 0 {
		sum := sha256.Sum256(e.Body)
		r.BodySHA256 = hex.EncodeToString(sum[:])
	}
	return r
}

// auditLog appends AuditRecords as JSON lines and rotates by size:
// file -> file.1 -> ... -> file.N. A nil *auditLog discards everything.
type auditLog struct {
	mu      sync.Mutex
	path    string
	max     int64
	keep    int
	f       *os.File
	size    int64
	log     *slog.Logger
	errored bool
}

func openAudit(path string, max int64, keep int, log *slog.Logger) (*auditLog, error) {
	a := &auditLog{path: path, max: max, keep: keep, log: log}
	if err := a.open(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *auditLog) open() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("audit directory: %w", err)
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	a.f, a.size = f, st.Size()
	return nil
}

func (a *auditLog) write(r AuditRecord) {
	if a == nil {
		return
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return
	}
	if a.size+int64(len(line)) > a.max && a.size > 0 {
		a.rotate()
	}
	n, err := a.f.Write(line)
	a.size += int64(n)
	if err != nil && !a.errored {
		a.errored = true // log once, not per record
		a.log.Error("audit write failed; further errors suppressed", "path", a.path, "err", err)
	}
}

// rotate shifts file.N-1 -> file.N ... file -> file.1 and reopens.
func (a *auditLog) rotate() {
	a.f.Close()
	for i := a.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1))
	}
	_ = os.Rename(a.path, a.path+".1")
	if err := a.open(); err != nil {
		a.f = nil
		a.log.Error("audit rotate failed; audit disabled", "path", a.path, "err", err)
	}
}

func (a *auditLog) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		_ = a.f.Sync()
		_ = a.f.Close()
		a.f = nil
	}
}
