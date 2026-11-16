package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

// Limits on the join endpoint. It is reachable without a token, so each
// one bounds what a stranger can make the broker store or do.
const (
	joinMaxPending      = 64       // requests waiting, in all
	joinMaxPendingPerIP = 4        // requests waiting from one address
	joinMaxBody         = 16 << 10 // bytes in one request
	joinRatePerMinute   = 60       // requests from one address
)

// Join statuses.
const (
	joinPending  = "pending"  // waiting for the operator
	joinApproved = "approved" // in the config: connect with the token
	joinUnknown  = "unknown"  // never asked, denied, or expired
)

// joinReply is the body of every answer from /join and /join/status.
type joinReply struct {
	Status string `json:"status,omitempty"`
	Agent  string `json:"agent,omitempty"`
	// Capabilities and SendTo are what the operator granted, once approved.
	Capabilities []string `json:"capabilities,omitempty"`
	SendTo       []string `json:"send_to,omitempty"`
	// MQTTPort and TLS say how to reach the bus on the host the client
	// already used for this request.
	MQTTPort string `json:"mqtt_port,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	Error    string `json:"error,omitempty"`
}

// joinServer takes join requests from clients that aren't agents yet and
// tells them how their request stands. It grants nothing: a request waits
// in the pending directory until an operator approves it, which adds the
// agent to the config file.
type joinServer struct {
	loc      confstore.Location
	mqttPort string
	tls      bool
	log      *slog.Logger

	mu   sync.Mutex // makes check-then-store one step, and guards hits
	hits map[string]*joinHits
	now  func() time.Time
}

type joinHits struct {
	start time.Time
	n     int
}

func newJoinServer(loc confstore.Location, mqttAddr string, tls bool, log *slog.Logger) *joinServer {
	_, port, _ := net.SplitHostPort(mqttAddr)
	return &joinServer{loc: loc, mqttPort: port, tls: tls, log: log.With("component", "join"),
		hits: map[string]*joinHits{}, now: time.Now}
}

func (s *joinServer) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /join", func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, true) })
	mux.HandleFunc("POST /join/status", func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, false) })
}

// allow counts a request against its address's budget for this minute.
func (s *joinServer) allow(ip string) bool {
	now := s.now()
	if len(s.hits) > 4096 { // forget idle addresses rather than grow forever
		for k, h := range s.hits {
			if now.Sub(h.start) > time.Minute {
				delete(s.hits, k)
			}
		}
	}
	h := s.hits[ip]
	if h == nil || now.Sub(h.start) > time.Minute {
		h = &joinHits{start: now}
		s.hits[ip] = h
	}
	h.n++
	return h.n <= joinRatePerMinute
}

func (s *joinServer) serve(w http.ResponseWriter, r *http.Request, create bool) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	s.mu.Lock()
	ok := s.allow(ip)
	s.mu.Unlock()
	if !ok {
		writeJoin(w, http.StatusTooManyRequests, joinReply{Error: "too many requests from this address; wait a minute"})
		return
	}
	// Read the body before taking the lock again: a slow sender must not
	// hold up everyone else's requests.
	var req confstore.JoinRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, joinMaxBody))
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeJoin(w, http.StatusBadRequest, joinReply{Error: "the request body must be JSON with id and token_sha256"})
		return
	}
	req.Normalize()
	if err := req.Validate(); err != nil {
		writeJoin(w, http.StatusBadRequest, joinReply{Error: err.Error()})
		return
	}
	// Remote and RequestedAt are the broker's own observations.
	req.Remote, req.RequestedAt = ip, s.now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	reply, err := s.status(req)
	switch {
	case errors.Is(err, errJoinTaken):
		writeJoin(w, http.StatusConflict, joinReply{Agent: req.ID, Error: err.Error()})
	case err != nil:
		s.log.Error("join request failed", "agent", req.ID, "remote", ip, "err", err)
		writeJoin(w, http.StatusInternalServerError, joinReply{Error: "the broker could not read its config"})
	case reply.Status == joinApproved:
		writeJoin(w, http.StatusOK, reply)
	case reply.Status == joinPending:
		writeJoin(w, http.StatusAccepted, reply)
	case !create:
		writeJoin(w, http.StatusNotFound, reply)
	default:
		s.create(w, req)
	}
}

var errJoinTaken = errors.New("that agent id is taken; pick another, or use the token that first asked for it")

// status says how a request stands: approved if the config has the agent
// with this token, pending if it's waiting, unknown otherwise. An ID held
// by a different token is errJoinTaken either way.
func (s *joinServer) status(req confstore.JoinRequest) (joinReply, error) {
	same := func(hash string) bool { return subtle.ConstantTimeCompare([]byte(hash), []byte(req.TokenSHA256)) == 1 }
	cfg, err := confstore.Load(s.loc.Path)
	if err != nil {
		return joinReply{}, err
	}
	if i := confstore.Find(cfg, req.ID); i >= 0 {
		a := cfg.Agents[i]
		if !same(a.TokenSHA256) {
			return joinReply{}, errJoinTaken
		}
		return joinReply{Status: joinApproved, Agent: a.ID, Capabilities: a.Capabilities, SendTo: a.SendTo,
			MQTTPort: s.mqttPort, TLS: s.tls}, nil
	}
	if p, ok := confstore.FindPending(s.loc, req.ID); ok {
		if !same(p.TokenSHA256) {
			return joinReply{}, errJoinTaken
		}
		return joinReply{Status: joinPending, Agent: p.ID}, nil
	}
	return joinReply{Status: joinUnknown, Agent: req.ID}, nil
}

// create stores a new request, within the limits on how many may wait.
func (s *joinServer) create(w http.ResponseWriter, req confstore.JoinRequest) {
	waiting, err := confstore.Pending(s.loc)
	if err != nil {
		s.log.Error("join request failed", "agent", req.ID, "remote", req.Remote, "err", err)
		writeJoin(w, http.StatusInternalServerError, joinReply{Error: "the broker could not store the request"})
		return
	}
	fromIP := 0
	for _, p := range waiting {
		if p.Remote == req.Remote {
			fromIP++
		}
	}
	switch {
	case fromIP >= joinMaxPendingPerIP:
		writeJoin(w, http.StatusTooManyRequests, joinReply{Error: fmt.Sprintf(
			"%d requests from this address are already waiting for the operator", fromIP)})
		return
	case len(waiting) >= joinMaxPending:
		s.log.Warn("join request refused: too many waiting", "agent", req.ID, "remote", req.Remote, "waiting", len(waiting))
		writeJoin(w, http.StatusServiceUnavailable, joinReply{Error: "too many join requests are waiting for the operator; try later"})
		return
	}
	if err := confstore.AddPending(s.loc, req); err != nil {
		s.log.Error("join request failed", "agent", req.ID, "remote", req.Remote, "err", err)
		writeJoin(w, http.StatusInternalServerError, joinReply{Error: "the broker could not store the request"})
		return
	}
	s.log.Info("join request waiting for approval", "agent", req.ID, "remote", req.Remote, "host", req.Host,
		"advertises", req.Capabilities, "approve", "agentbus config join approve "+req.ID)
	writeJoin(w, http.StatusAccepted, joinReply{Status: joinPending, Agent: req.ID})
}

func writeJoin(w http.ResponseWriter, code int, reply joinReply) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(reply)
}

// startJoinEndpoint serves the join endpoint on join_listen, for a config
// where it isn't sharing the MCP endpoint's address.
func startJoinEndpoint(s *joinServer, cfg *broker.Config, log *slog.Logger) (stop func(), addr string, err error) {
	mux := http.NewServeMux()
	s.mount(mux)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	ln, err := listenHTTP(cfg.JoinListen, cfg, srv)
	if err != nil {
		return nil, "", fmt.Errorf("join_listen: %w", err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("join endpoint stopped", "err", err)
		}
	}()
	log.Info("join requests over HTTP", "addr", ln.Addr().String(), "path", "/join", "tls", cfg.TLSCert != "")
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, ln.Addr().String(), nil
}
