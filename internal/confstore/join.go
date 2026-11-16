package confstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

// Join requests wait in a directory beside the config, one file each:
//
//	~/.config/agentbus/pending/<id>.json  (0600)
//
// The broker writes them when a client asks to join (join_listen), and
// the operator approves or denies them with "agentbus config join" or the
// config editor. Approving adds the agent to the config with the token
// hash the client sent, so the token itself never leaves the client.

// Limits on a join request.
const (
	// JoinTTL is how long an unanswered request is kept.
	JoinTTL = 24 * time.Hour
	// MaxJoinCapabilities caps the capabilities one request may advertise.
	MaxJoinCapabilities = 32
	// MaxJoinText caps the description and host name, in bytes.
	MaxJoinText = 200
)

// JoinRequest is a client's request to become an agent. Everything in it
// except Remote and RequestedAt comes from the client and is untrusted.
type JoinRequest struct {
	// ID is the agent ID the client wants.
	ID string `json:"id"`
	// TokenSHA256 is the hex SHA-256 of the token the client generated.
	TokenSHA256 string `json:"token_sha256"`
	// Capabilities are the task queues the client would like to serve.
	// They are a request: the operator decides what to grant.
	Capabilities []string `json:"capabilities,omitempty"`
	// Description and Host are labels for the operator.
	Description string `json:"description,omitempty"`
	Host        string `json:"host,omitempty"`
	// Remote is the address the request came from, as the broker saw it.
	Remote      string    `json:"remote,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

// Normalize trims the request's labels, drops characters that don't
// print, and sorts and dedupes its capabilities.
func (r *JoinRequest) Normalize() {
	r.ID = strings.TrimSpace(r.ID)
	r.TokenSHA256 = strings.ToLower(strings.TrimSpace(r.TokenSHA256))
	r.Description, r.Host = label(r.Description), label(r.Host)
	caps := slices.Clone(r.Capabilities)
	slices.Sort(caps)
	r.Capabilities = slices.Compact(caps)
}

// label makes client-supplied text safe to store and show on one line.
func label(s string) string {
	s = strings.Map(func(c rune) rune {
		if c == unicode.ReplacementChar || !unicode.IsPrint(c) {
			return -1
		}
		return c
	}, strings.TrimSpace(s))
	for len(s) > MaxJoinText {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// Validate checks a normalized request.
func (r *JoinRequest) Validate() error {
	if !a2a.ValidName(r.ID) {
		return fmt.Errorf("invalid agent id %q: use 1-64 of a-z 0-9 . _ -", r.ID)
	}
	if b, err := hex.DecodeString(r.TokenSHA256); err != nil || len(b) != sha256.Size {
		return errors.New("token_sha256 must be 64 hex chars: the SHA-256 of the token the client keeps")
	}
	if len(r.Capabilities) > MaxJoinCapabilities {
		return fmt.Errorf("too many capabilities: at most %d", MaxJoinCapabilities)
	}
	for _, c := range r.Capabilities {
		// No "*": an operator may grant every queue, a client may not ask.
		if !a2a.ValidName(c) {
			return fmt.Errorf("invalid capability %q: use 1-64 of a-z 0-9 . _ -", c)
		}
	}
	return nil
}

// Expired reports whether the request is older than JoinTTL.
func (r *JoinRequest) Expired(now time.Time) bool { return now.Sub(r.RequestedAt) > JoinTTL }

// From describes where a request came from, for display.
func (r *JoinRequest) From() string {
	switch {
	case r.Host != "" && r.Remote != "":
		return r.Host + " (" + r.Remote + ")"
	case r.Host != "":
		return r.Host
	}
	return r.Remote
}

// PendingDir holds the join requests for the config at l.
func (l Location) PendingDir() string { return filepath.Join(filepath.Dir(l.Path), "pending") }

// PendingFile is where the join request for id is kept.
func (l Location) PendingFile(id string) string { return filepath.Join(l.PendingDir(), id+".json") }

// Pending lists the join requests waiting for an answer, oldest first.
// Requests older than JoinTTL, and files that don't parse, are deleted.
func Pending(loc Location) ([]JoinRequest, error) {
	entries, err := os.ReadDir(loc.PendingDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []JoinRequest
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue // a temp file being written, or not ours
		}
		r, err := readPending(loc, id)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || r.Expired(now) {
			_ = os.Remove(loc.PendingFile(id))
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b JoinRequest) int {
		if c := a.RequestedAt.Compare(b.RequestedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func readPending(loc Location, id string) (JoinRequest, error) {
	var r JoinRequest
	data, err := os.ReadFile(loc.PendingFile(id))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.ID != id {
		return r, fmt.Errorf("%s holds a request for %q", loc.PendingFile(id), r.ID)
	}
	return r, r.Validate()
}

// FindPending returns the waiting join request for id.
func FindPending(loc Location, id string) (JoinRequest, bool) {
	if !a2a.ValidName(id) {
		return JoinRequest{}, false
	}
	r, err := readPending(loc, id)
	if err != nil || r.Expired(time.Now()) {
		return JoinRequest{}, false
	}
	return r, true
}

// AddPending stores a join request. It refuses one whose ID is already
// waiting: the first request for an ID holds it until answered.
func AddPending(loc Location, r JoinRequest) error {
	r.Normalize()
	if err := r.Validate(); err != nil {
		return err
	}
	if _, ok := FindPending(loc, r.ID); ok {
		return fmt.Errorf("a join request for %q is already waiting", r.ID)
	}
	data, err := json.MarshalIndent(&r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(loc.PendingFile(r.ID), append(data, '\n'))
}

// RemovePending deletes the join request for id: the request is denied.
func RemovePending(loc Location, id string) error {
	if !a2a.ValidName(id) {
		return fmt.Errorf("no join request for %q", id)
	}
	err := os.Remove(loc.PendingFile(id))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no join request for %q", id)
	}
	return err
}

// ApproveJoin adds the agent that asked to join as id, with the token
// hash from its request and the grants in a (a's ID and token hash are
// ignored). Nothing the client advertised is granted unless a says so.
// The request is deleted when the Tx commits. Unlike AddAgent there is no
// token file: the token exists only on the client.
func (tx *Tx) ApproveJoin(id string, a broker.AgentConfig) error {
	r, ok := FindPending(tx.loc, id)
	if !ok {
		return fmt.Errorf("no join request for %q", id)
	}
	if Find(tx.Config, id) >= 0 {
		return fmt.Errorf("agent %q already exists; deny the request, or remove the agent first", id)
	}
	a.ID, a.TokenSHA256 = r.ID, r.TokenSHA256
	tx.Config.Agents = append(tx.Config.Agents, a)
	tx.remove = append(tx.remove, tx.loc.PendingFile(id))
	return nil
}
