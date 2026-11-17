//go:build !windows

package exec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

func sh(script string) StartParams { return StartParams{Argv: []string{"/bin/sh", "-c", script}} }

func TestExitComesLastAfterAllOutput(t *testing.T) {
	m, c := newManager()
	pid := c.run(t, m, sh("echo out; echo err >&2; exit 3"))
	stdout, stderr, methods, code := c.forPid(pid)
	if string(stdout) != "out\n" || string(stderr) != "err\n" || code != 3 {
		t.Fatalf("stdout=%q stderr=%q code=%d", stdout, stderr, code)
	}
	if methods[len(methods)-1] != "exec.exit" || strings.Count(strings.Join(methods, ","), "exec.exit") != 1 {
		t.Fatalf("notifications = %v, want exec.exit exactly once and last", methods)
	}
}

func TestLargeOutputIsChunkedAndIntact(t *testing.T) {
	m, c := newManager()
	const total = 300000
	pid := c.run(t, m, sh("head -c "+strconv.Itoa(total)+" /dev/zero | tr '\\0' 'x'"))
	stdout, _, methods, _ := c.forPid(pid)
	if len(stdout) != total || strings.Trim(string(stdout), "x") != "" {
		t.Fatalf("got %d bytes", len(stdout))
	}
	if methods[len(methods)-1] != "exec.exit" {
		t.Fatal("exit not last")
	}
}

func TestStdin(t *testing.T) {
	m, c := newManager()
	p := StartParams{Argv: []string{"cat"}, Stdin: true}
	res, err := m.Start(p)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)
	m.Stdin(StdinParams{Pid: res.Pid, Data: []byte("hello ")})
	m.Stdin(StdinParams{Pid: res.Pid, Data: []byte("world"), EOF: true})
	<-done
	if out, _, _, _ := c.forPid(res.Pid); string(out) != "hello world" {
		t.Fatalf("stdout = %q", out)
	}
	if _, err := m.Stdin(StdinParams{Pid: res.Pid}); err == nil {
		t.Fatal("stdin accepted for an exited process")
	}
}

func TestStdinClosedWithoutRequest(t *testing.T) {
	m, c := newManager()
	pid := c.run(t, m, sh("cat; echo done")) // returns only if stdin is already closed
	if out, _, _, _ := c.forPid(pid); string(out) != "done\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestKillTakesTheWholeTree(t *testing.T) {
	m, c := newManager()
	res, err := m.Start(sh("sleep 60 & echo $!; wait"))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)

	var grandchild int
	for deadline := time.Now().Add(5 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out, _, _, _ := c.forPid(res.Pid)
		grandchild, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if grandchild == 0 {
		t.Fatal("never saw the grandchild pid")
	}
	if _, err := m.Kill(KillParams{Pid: res.Pid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no exec.exit after kill")
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(grandchild, 0) == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if syscall.Kill(grandchild, 0) == nil {
		t.Fatal("grandchild survived the kill")
	}
	if _, err := m.Kill(KillParams{Pid: res.Pid}); errCode(err) != proto.CodeNotFound {
		t.Fatalf("second kill: %v", err)
	}
}

// The host lost the connection before the exec.start reply reached it: it has
// no pid to read or kill, so the Guest Service ends the whole tree itself and
// says nothing about it.
func TestAnUnansweredStartEndsTheTreeWithoutNotifications(t *testing.T) {
	m, c := newManager()
	file := filepath.Join(t.TempDir(), "grandchild")
	res, err := m.Start(sh("sleep 60 & echo $! > " + file + "; wait"))
	if err != nil {
		t.Fatal(err)
	}
	var grandchild int
	for deadline := time.Now().Add(5 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, _ := os.ReadFile(file)
		grandchild, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if grandchild == 0 {
		t.Fatal("never saw the grandchild pid")
	}
	res.Sent(false)
	if _, err := m.Kill(KillParams{Pid: res.Pid}); errCode(err) != proto.CodeNotFound {
		t.Fatalf("kill after an unanswered start: %v", err)
	}
	for _, pid := range []int{res.Pid, grandchild} {
		for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		}
		if syscall.Kill(pid, 0) == nil {
			t.Fatalf("pid %d survived (the child unreaped, or the grandchild unkilled)", pid)
		}
	}
	if _, _, methods, _ := c.forPid(res.Pid); len(methods) != 0 {
		t.Fatalf("notifications for a pid the host never saw: %v", methods)
	}
}

func TestConcurrentProcessesKeepTheirOutputApart(t *testing.T) {
	m, c := newManager()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			n := strconv.Itoa(i)
			pid := c.run(t, m, sh("for j in 1 2 3; do echo "+n+"; done"))
			if out, _, _, _ := c.forPid(pid); string(out) != strings.Repeat(n+"\n", 3) {
				t.Errorf("proc %s stdout = %q", n, out)
			}
		})
	}
	wg.Wait()
}

func TestCwdAndEnv(t *testing.T) {
	m, c := newManager()
	dir := t.TempDir()
	p := sh(`pwd -P; echo "$GP_TEST"`)
	p.Cwd, p.Env = dir, map[string]string{"GP_TEST": "v1"}
	pid := c.run(t, m, p)
	out, _, _, _ := c.forPid(pid)
	lines := strings.Fields(string(out))
	if len(lines) != 2 || !strings.HasSuffix(lines[0], dir[strings.LastIndex(dir, "/")+1:]) || lines[1] != "v1" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestStartErrors(t *testing.T) {
	m, _ := newManager()
	for _, tc := range []struct {
		name string
		p    StartParams
		code int
	}{
		{"empty argv", StartParams{}, proto.CodeInvalidParams},
		{"bad as", StartParams{Argv: []string{"true"}, As: "root"}, proto.CodeInvalidParams},
		{"missing program", StartParams{Argv: []string{"/nonexistent/prog"}}, proto.CodeNotFound},
		{"missing cwd", StartParams{Argv: []string{"true"}, Cwd: "/nonexistent"}, proto.CodeNotFound},
	} {
		if _, err := m.Start(tc.p); errCode(err) != tc.code {
			t.Errorf("%s: err = %v, want code %d", tc.name, err, tc.code)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"Path=C:\\a", "=C:=C:\\dir", "X=1"}
	got := mergeEnv(base, map[string]string{"PATH": "C:\\b", "NEW": "2"}, true)
	want := map[string]bool{"PATH=C:\\b": true, "=C:=C:\\dir": true, "X=1": true, "NEW=2": true}
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("unexpected %q in %v", kv, got)
		}
	}
	if got := mergeEnv([]string{"a=1"}, map[string]string{"A": "2"}, false); len(got) != 2 {
		t.Errorf("case-sensitive merge = %v", got)
	}
}

func TestKilledProcessExitsWith128PlusSignal(t *testing.T) {
	m, c := newManager()
	pid := c.run(t, m, sh("kill -KILL $$"))
	if _, _, _, code := c.forPid(pid); code != 137 {
		t.Fatalf("code = %d, want 137", code)
	}
}
