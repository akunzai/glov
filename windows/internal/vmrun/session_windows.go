package vmrun

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/guest"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// Timings from docs/guest-protocol.md (Heartbeat): guest.info every 5 s,
// 3 s for each answer, two misses in a row make the session unresponsive.
const (
	heartbeatEvery   = 5 * time.Second
	heartbeatTimeout = 3 * time.Second
	heartbeatMisses  = 2
	// dialTimeout is short: a connect sent before the guest listens hangs
	// for 30 s (see guest.Dial), and the guest listens about 2 s after boot.
	dialTimeout = time.Second
	redialWait  = 250 * time.Millisecond
)

// guestInfo is guest.info's result and guest.ready's params.
type guestInfo struct {
	Version string  `json:"version"`
	OS      string  `json:"os"`
	User    *string `json:"user"`
	BootID  string  `json:"bootId"`
}

// session is the host's connection to one VM's Guest Service.
type session struct {
	jobs *jobs // the VM's, kept across boots

	mu     sync.Mutex
	state  string // "connecting", "ready" or "unresponsive"
	info   *guestInfo
	silent time.Time   // when the last answer came, while unresponsive
	client *rpc.Client // while connected
	conns  int         // connections established, for ConnectionID
	// onReady runs once each connection is ready.
	onReady func()
	// onNotify receives the notifications notify does not handle.
	onNotify func(method string, params jsontext.Value)
}

func newSession(j *jobs, onReady func()) *session {
	return &session{state: "connecting", jobs: j, onReady: onReady}
}

// guest returns the connection to a Guest Service that answers, and the
// boot it answers from.
func (s *session) guest() (*rpc.Client, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == "unresponsive":
		return nil, "", errors.New("the Guest Service is not responding")
	case s.state != "ready" || s.client == nil:
		return nil, "", errors.New("the Guest Service is not ready yet: glov vm wait <vm> guest-ready")
	}
	return s.client, s.info.BootID, nil
}

func (s *session) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == "ready"
}

func (s *session) set(state string, info *guestInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == "unresponsive" && s.state != "unresponsive" {
		s.silent = time.Now()
	}
	s.state = state
	if info != nil {
		if s.info == nil || s.info.BootID != info.BootID {
			s.jobs.rebooted(info.BootID)
		}
		s.info = info
	}
}

// status is the Guest Service part of vm.status.
func (s *session) status() *api.GuestService {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &api.GuestService{State: s.state}
	if s.info != nil && s.state != "connecting" {
		g.OS, g.Version = &s.info.OS, &s.info.Version
		if s.state == "ready" {
			g.User, g.BootID = s.info.User, &s.info.BootID
			if s.client != nil {
				id := strconv.Itoa(s.conns)
				g.ConnectionID = &id
			}
		}
	}
	if s.state == "unresponsive" {
		p := fmt.Sprintf("no answer for %s", time.Since(s.silent).Round(time.Second))
		g.Problem = &p
	}
	return g
}

// run keeps a connection to the Guest Service until ctx ends.
func (s *session) run(ctx context.Context, vmID guid.GUID) {
	for ctx.Err() == nil {
		s.connect(ctx, vmID)
		if ctx.Err() == nil {
			s.set("connecting", nil)
			select {
			case <-ctx.Done():
			case <-time.After(redialWait):
			}
		}
	}
}

// connect dials once and, if the Guest Service answers, heartbeats until
// the connection ends.
func (s *session) connect(ctx context.Context, vmID guid.GUID) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	c, err := guest.Dial(dctx, vmID, s.notify)
	cancel()
	if err != nil {
		return
	}
	defer c.Close()
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if !s.heartbeat(ctx, c) {
		return
	}
	s.mu.Lock()
	s.client = c
	s.conns++
	s.mu.Unlock()
	if s.onReady != nil {
		go s.onReady()
	}
	misses := 0
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-c.Done():
			return
		case <-t.C:
		}
		if s.heartbeat(ctx, c) {
			misses = 0
		} else if misses++; misses >= heartbeatMisses {
			s.set("unresponsive", nil)
		}
	}
}

// notify handles the Guest Service's notifications.
func (s *session) notify(method string, params jsontext.Value) {
	switch method {
	case "guest.ready":
		var info guestInfo
		if json.Unmarshal(params, &info) == nil {
			s.set("ready", &info)
		}
	case "exec.output":
		var p struct {
			PID  int    `json:"pid"`
			Data []byte `json:"data"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.jobs.output(p.PID, p.Data)
		}
	case "exec.exit":
		var p struct {
			PID  int `json:"pid"`
			Code int `json:"code"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.jobs.exit(p.PID, p.Code)
		}
	default:
		if s.onNotify != nil {
			s.onNotify(method, params)
		}
	}
}

// heartbeat calls guest.info and records the answer.
func (s *session) heartbeat(ctx context.Context, c *rpc.Client) bool {
	cctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	var info guestInfo
	if err := c.Call(cctx, "guest.info", nil, &info); err != nil {
		return false
	}
	s.set("ready", &info)
	return true
}
