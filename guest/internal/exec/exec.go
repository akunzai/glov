// Package exec implements the exec.* methods: concurrent child processes whose
// output streams back as notifications. Output and environment values are
// never logged.
package exec

import (
	"io"
	"log/slog"
	"sync"

	"github.com/akunzai/glov/guest/proto"
)

// chunkSize is the largest raw payload of one exec.output notification.
const chunkSize = 65536

// Notifier delivers a notification, blocking until it is written. It must be
// done with params when it returns (the pump reuses its buffer).
type Notifier func(method string, params any)

type StartParams struct {
	Argv  []string          `json:"argv"`
	Cwd   string            `json:"cwd"`
	Env   map[string]string `json:"env"`
	As    string            `json:"as"`
	Stdin bool              `json:"stdin"`
}

// StartResult is the exec.start result. Output starts flowing only after the
// response carrying the pid is on the wire (see proto.Sender).
type StartResult struct {
	Pid   int `json:"pid"`
	begin func(delivered bool)
}

func (r StartResult) Sent(delivered bool) { r.begin(delivered) }

type StdinParams struct {
	Pid  int    `json:"pid"`
	Data []byte `json:"data"`
	EOF  bool   `json:"eof"`
}

type KillParams struct {
	Pid int `json:"pid"`
}

type outputParams struct {
	Pid    int    `json:"pid"`
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

type exitParams struct {
	Pid  int `json:"pid"`
	Code int `json:"code"`
}

// child is a started process, however it was created.
type child interface {
	Pid() int
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() int   // exit code; called once both streams hit EOF
	Kill() error // the whole process tree
	Close()
}

type proc struct {
	child
	stdin *stdinQueue // nil unless requested
}

type Manager struct {
	notify Notifier
	mu     sync.Mutex
	procs  map[int]*proc
}

func NewManager(notify Notifier) *Manager {
	return &Manager{notify: notify, procs: map[int]*proc{}}
}

func (m *Manager) Start(p StartParams) (StartResult, error) {
	if len(p.Argv) == 0 || p.Argv[0] == "" {
		return StartResult{}, proto.Errorf(proto.CodeInvalidParams, "argv is required")
	}
	var (
		c   child
		in  io.WriteCloser
		err error
	)
	switch p.As {
	case "", "system":
		c, in, err = startSystem(p)
	case "user":
		c, in, err = startUser(p)
	default:
		return StartResult{}, proto.Errorf(proto.CodeInvalidParams, `as must be "system" or "user"`)
	}
	if err != nil {
		return StartResult{}, proto.FromOS(err)
	}
	pr := &proc{child: c}
	if in != nil {
		pr.stdin = newStdinQueue(in)
	}
	m.mu.Lock()
	m.procs[c.Pid()] = pr
	m.mu.Unlock()
	return StartResult{Pid: c.Pid(), begin: func(delivered bool) {
		if delivered {
			go m.run(pr)
		} else {
			m.discard(pr)
		}
	}}, nil
}

// discard ends a process whose exec.start reply never reached the host: with
// no pid, the host can neither read its output nor kill it, so the tree is
// killed here, and reaped in the background, without notifications for a pid
// the host never saw.
func (m *Manager) discard(pr *proc) {
	pid := pr.Pid()
	m.mu.Lock()
	delete(m.procs, pid)
	m.mu.Unlock()
	if err := pr.Kill(); err != nil {
		slog.Warn("cannot kill a process whose start was not answered", "pid", pid, "err", err)
	}
	go func() {
		var wg sync.WaitGroup
		wg.Go(func() { io.Copy(io.Discard, pr.Stdout()) })
		wg.Go(func() { io.Copy(io.Discard, pr.Stderr()) })
		wg.Wait()
		pr.Wait()
		if pr.stdin != nil {
			pr.stdin.close()
		}
		pr.Close()
	}()
}

// run pumps both streams to EOF, then reports the exit: exec.exit is the last
// notification of a pid because it is sent only after both pumps returned.
func (m *Manager) run(pr *proc) {
	pid := pr.Pid()
	var wg sync.WaitGroup
	wg.Go(func() { m.pump(pid, "stdout", pr.Stdout()) })
	wg.Go(func() { m.pump(pid, "stderr", pr.Stderr()) })
	wg.Wait()
	code := pr.Wait()
	m.notify("exec.exit", exitParams{pid, code})
	m.mu.Lock()
	delete(m.procs, pid)
	m.mu.Unlock()
	if pr.stdin != nil {
		pr.stdin.close()
	}
	pr.Close()
}

func (m *Manager) pump(pid int, stream string, r io.Reader) {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.notify("exec.output", outputParams{pid, stream, buf[:n]})
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) lookup(pid int) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.procs[pid]
	if !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no such process: %d", pid)
	}
	return pr, nil
}

func (m *Manager) Stdin(p StdinParams) (struct{}, error) {
	pr, err := m.lookup(p.Pid)
	if err != nil {
		return struct{}{}, err
	}
	if pr.stdin == nil {
		return struct{}{}, proto.Errorf(proto.CodeInvalidParams, "process was not started with stdin")
	}
	if !pr.stdin.push(p.Data, p.EOF) {
		return struct{}{}, proto.Errorf(proto.CodeInvalidParams, "stdin is closed")
	}
	return struct{}{}, nil
}

// Kill terminates the process tree; the exec.exit notification still follows.
func (m *Manager) Kill(p KillParams) (struct{}, error) {
	pr, err := m.lookup(p.Pid)
	if err != nil {
		return struct{}{}, err
	}
	if err := pr.Kill(); err != nil {
		return struct{}{}, proto.FromOS(err)
	}
	return struct{}{}, nil
}
