package confstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/broker"
)

func joinLoc(t *testing.T) Location {
	t.Helper()
	loc := Location{Path: filepath.Join(t.TempDir(), "broker.json"), Source: FromFlag}
	if _, err := Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	return loc
}

func joinReq(id string, caps ...string) JoinRequest {
	_, hash := broker.NewToken()
	return JoinRequest{ID: id, TokenSHA256: hash, Capabilities: caps, RequestedAt: time.Now()}
}

func TestJoinRequestValidation(t *testing.T) {
	for name, r := range map[string]JoinRequest{
		"bad id":             {ID: "Not Valid", TokenSHA256: joinReq("x").TokenSHA256},
		"short hash":         {ID: "w", TokenSHA256: "abc"},
		"wildcard":           joinReq("w", "*"),
		"bad capability":     joinReq("w", "a/b"),
		"too many":           joinReq("w", strings.Fields(strings.Repeat("c ", MaxJoinCapabilities+1))...),
		"path as capability": joinReq("w", "../x"),
	} {
		if name == "too many" {
			for i := range r.Capabilities {
				r.Capabilities[i] = "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			}
		}
		r.Normalize()
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}

	r := joinReq("worker", "b", "a", "b")
	r.Description = "  build\x1b[31m box\nline two " + strings.Repeat("é", 300)
	r.Host = "host\x00name"
	r.Normalize()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Capabilities, ",") != "a,b" {
		t.Errorf("capabilities = %v", r.Capabilities)
	}
	if strings.ContainsAny(r.Description, "\x1b\n") || len(r.Description) > MaxJoinText || r.Host != "hostname" {
		t.Errorf("labels not cleaned: %q %q", r.Description, r.Host)
	}
}

func TestPendingLifecycle(t *testing.T) {
	loc := joinLoc(t)
	if reqs, err := Pending(loc); err != nil || len(reqs) != 0 {
		t.Fatalf("no pending dir: %v %v", reqs, err)
	}
	first := joinReq("worker", "summarize", "translate")
	first.RequestedAt = time.Now().Add(-time.Minute)
	if err := AddPending(loc, first); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(loc.PendingFile("worker")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pending file: %v %v", fi, err)
	}
	// The first request for an ID holds it.
	if err := AddPending(loc, joinReq("worker")); err == nil {
		t.Error("a second request replaced the first")
	}
	if err := AddPending(loc, joinReq("other")); err != nil {
		t.Fatal(err)
	}
	// An old request, and a file that isn't a request, are dropped.
	old := joinReq("stale")
	old.RequestedAt = time.Now().Add(-JoinTTL - time.Minute)
	data, _ := json.Marshal(old)
	_ = os.WriteFile(loc.PendingFile("stale"), data, 0o600)
	_ = os.WriteFile(loc.PendingFile("junk"), []byte("{"), 0o600)
	_ = os.WriteFile(loc.PendingFile("liar"), []byte(strings.Replace(string(data), "stale", "admin", 1)), 0o600)

	reqs, err := Pending(loc)
	if err != nil || len(reqs) != 2 || reqs[0].ID != "worker" || reqs[1].ID != "other" {
		t.Fatalf("pending = %+v, %v", reqs, err)
	}
	for _, id := range []string{"stale", "junk", "liar"} {
		if _, err := os.Stat(loc.PendingFile(id)); err == nil {
			t.Errorf("%s was not removed", id)
		}
	}
	if _, ok := FindPending(loc, "../broker"); ok {
		t.Error("FindPending followed a path")
	}

	if err := RemovePending(loc, "other"); err != nil {
		t.Fatal(err)
	}
	if err := RemovePending(loc, "other"); err == nil {
		t.Error("denying twice did not fail")
	}
}

func TestApproveJoin(t *testing.T) {
	loc := joinLoc(t)
	req := joinReq("worker", "summarize", "translate")
	if err := AddPending(loc, req); err != nil {
		t.Fatal(err)
	}

	// The grant is the operator's: here, one of the two it asked for. A
	// token hash or ID passed in the grant is ignored.
	tx, err := Begin(loc)
	if err != nil {
		t.Fatal(err)
	}
	grant := broker.AgentConfig{ID: "admin2", TokenSHA256: "ff", Capabilities: []string{"summarize"}, SendTo: []string{"admin"}}
	if err := tx.ApproveJoin("worker", grant); err != nil {
		t.Fatal(err)
	}
	if err := tx.ApproveJoin("nobody", grant); err == nil {
		t.Error("approved a request that doesn't exist")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(loc.Path)
	i := Find(cfg, "worker")
	if i < 0 {
		t.Fatal("agent not added")
	}
	a := cfg.Agents[i]
	if a.TokenSHA256 != req.TokenSHA256 || strings.Join(a.Capabilities, ",") != "summarize" || a.Observer || a.SendTo[0] != "admin" {
		t.Errorf("agent = %+v", a)
	}
	if _, ok := FindPending(loc, "worker"); ok {
		t.Error("the request is still pending after approval")
	}
	if _, ok := ReadToken(loc, "worker"); ok {
		t.Error("a token file appeared on the broker: the token belongs to the client")
	}

	// An ID that became an agent some other way can't be approved over.
	if err := AddPending(loc, joinReq("admin")); err != nil {
		t.Fatal(err)
	}
	tx, _ = Begin(loc)
	if err := tx.ApproveJoin("admin", broker.AgentConfig{}); err == nil {
		t.Error("approved a request for an existing agent's ID")
	}
	tx.Abort()

	// An aborted approval leaves the request waiting.
	if err := AddPending(loc, joinReq("later")); err != nil {
		t.Fatal(err)
	}
	tx, _ = Begin(loc)
	if err := tx.ApproveJoin("later", broker.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	tx.Abort()
	if _, ok := FindPending(loc, "later"); !ok {
		t.Error("abort removed the request")
	}
}
