// Package guestservice is the Guest Service itself: it wires the protocol
// methods to their implementations and keeps the transport connected.
package guestservice

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"time"

	"github.com/akunzai/glov/guest/internal/exec"
	"github.com/akunzai/glov/guest/internal/files"
	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/internal/secrets"
	"github.com/akunzai/glov/guest/internal/stats"
	"github.com/akunzai/glov/guest/internal/transport"
	"github.com/akunzai/glov/guest/proto"
)

const protocolVersion = 1

// Info is the guest.info result and the guest.ready notification params.
type Info struct {
	ProtocolVersion int     `json:"protocolVersion"`
	Version         string  `json:"version"`
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
	Hostname        string  `json:"hostname"`
	User            *string `json:"user"`
	BootID          string  `json:"bootId"`
}

type Service struct {
	version string
	hub     *hub
	server  *proto.Server
}

func New(version string) *Service {
	svc := &Service{version: version, hub: newHub(), server: proto.NewServer()}
	mgr := exec.NewManager(svc.hub.notify)
	s := svc.server
	s.Handle("guest.info", func(struct{}) (Info, error) { return svc.info(), nil })
	// It samples CPU use for half a second: beside the loop, so heartbeats are not held up.
	s.HandleAsync("guest.stats", func(struct{}) (stats.Stats, error) {
		st, err := stats.Collect()
		if err != nil {
			return st, proto.FromOS(err)
		}
		return st, nil
	})
	s.Handle("exec.start", mgr.Start)
	s.Handle("exec.stdin", mgr.Stdin)
	s.Handle("exec.kill", mgr.Kill)
	// Reads may block on a slow path, so they run beside the request loop.
	// Writes and removes stay inline: a host that pipelines chunks relies on
	// their order.
	s.HandleAsync("file.stat", files.Stat)
	s.HandleAsync("file.list", files.List)
	s.HandleAsync("file.read", files.Read)
	s.Handle("file.write", files.Write)
	s.Handle("file.remove", files.Remove)
	// Windows only: the host reboots a Linux guest by stopping and starting
	// it, which works even when the Guest Service does not answer.
	if prepareReboot != nil {
		s.Handle("power.reboot", func(struct{}) (rebootResult, error) { return newReboot() })
	}
	s.Handle("time.sync", func(p timeSyncParams) (struct{}, error) { return struct{}{}, syncTime(p) })
	// Linux only: harness VMs are Linux. Other guests answer -32601.
	if runtime.GOOS == "linux" {
		s.Handle("secrets.sync", secrets.New().Sync)
	}
	// A listing hashes and a batch writes many files: both run beside the
	// request loop so heartbeats keep being answered. The host sends one at a
	// time per share, and the manager serialises each share's calls.
	mirrors := mirror.NewManager(mirror.StateDir, mirror.PrepareStateDir)
	s.HandleAsync("sync.manifest", mirrors.Manifest)
	s.HandleAsync("sync.apply", mirrors.Apply)
	// Inline, so that a cancel reaches the share before any later call does.
	s.Handle("sync.status", mirrors.Status)
	s.Handle("sync.cancel", mirrors.Cancel)
	// Linux only (live_linux.go): live shares are a Windows host's, whose
	// guests are Linux. Other guests answer -32601.
	registerLive(s, mirrors, svc.hub.notify)
	return svc
}

func (s *Service) info() Info {
	host, user, boot := hostInfo()
	return Info{protocolVersion, s.version, runtime.GOOS, runtime.GOARCH, host, user, boot}
}

// rebootResult restarts the guest once the reply is on the wire. An undelivered
// reply restarts nothing: the host, which never heard back, asks again.
type rebootResult struct{ do func() }

func (rebootResult) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }
func (r rebootResult) Sent(delivered bool) {
	if delivered {
		go r.do()
	}
}

func newReboot() (rebootResult, error) {
	do, err := prepareReboot()
	if err != nil {
		return rebootResult{}, proto.FromOS(err)
	}
	return rebootResult{func() {
		if err := do(); err != nil {
			slog.Error("reboot failed", "err", err)
		}
	}}, nil
}

type timeSyncParams struct {
	UnixMillis int64 `json:"unixMillis"`
}

func syncTime(p timeSyncParams) error {
	if p.UnixMillis <= 0 {
		return proto.Errorf(proto.CodeInvalidParams, "unixMillis must be positive")
	}
	if err := setSystemTime(time.UnixMilli(p.UnixMillis)); err != nil {
		return proto.FromOS(err)
	}
	return nil
}

// Run serves the protocol over streams from open until ctx ends. When the
// stream fails or cannot be opened it tries again after retry, forever; the
// host going away is normal, never fatal. A zero retry serves a single
// stream and returns when it ends.
func (s *Service) Run(ctx context.Context, open func() (io.ReadWriteCloser, error), retry time.Duration) error {
	defer s.hub.shutdown()
	var lastErr string
	for {
		rwc, err := open()
		if err != nil {
			if msg := err.Error(); msg != lastErr { // one line per distinct failure, not one per retry
				slog.Info("port not available", "err", msg)
				lastErr = msg
			}
		} else {
			lastErr = ""
			s.serve(ctx, rwc)
		}
		if retry == 0 {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// RunListener serves the host connections that l accepts, one at a time, until
// ctx ends. A new connection replaces the current one: after a snapshot
// restore the guest never sees the old connection close, so waiting for it
// would leave the reconnecting host stuck in the backlog. The replacement gets
// guest.ready like any other session. Accept errors other than the listener
// being closed are retried after retry.
func (s *Service) RunListener(ctx context.Context, l transport.Listener, retry time.Duration) {
	defer s.hub.shutdown()
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var cur io.Closer
	var done chan struct{}
	end := func() { // close the current session and wait until it is torn down
		if cur != nil {
			cur.Close()
			<-done
			cur = nil
		}
	}
	defer end()
	for {
		rwc, err := l.Accept()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("accept failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			continue
		}
		end()
		cur, done = rwc, make(chan struct{})
		go func() { defer close(done); s.serve(ctx, rwc) }()
	}
}

func (s *Service) serve(ctx context.Context, rwc io.ReadWriteCloser) {
	c := proto.NewConn(rwc)
	defer s.hub.drop(c)
	if err := c.Notify("guest.ready", s.info()); err != nil {
		slog.Warn("guest.ready failed", "err", err)
		return
	}
	slog.Info("connected")
	s.hub.set(c)
	err := s.server.Serve(ctx, c)
	slog.Info("disconnected", "err", err)
}
