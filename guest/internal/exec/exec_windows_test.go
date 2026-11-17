package exec

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/guest/proto"
)

func cmdc(script string) StartParams {
	return StartParams{Argv: []string{"cmd.exe", "/c", script}}
}

func TestWindowsExitComesLastAfterAllOutput(t *testing.T) {
	m, c := newManager()
	pid := c.run(t, m, cmdc("echo out& echo err 1>&2& exit 3"))
	stdout, stderr, methods, code := c.forPid(pid)
	if strings.TrimSpace(string(stdout)) != "out" || strings.TrimSpace(string(stderr)) != "err" || code != 3 {
		t.Fatalf("stdout=%q stderr=%q code=%d", stdout, stderr, code)
	}
	if methods[len(methods)-1] != "exec.exit" {
		t.Fatalf("notifications = %v", methods)
	}
}

func TestWindowsEnvAndCwd(t *testing.T) {
	m, c := newManager()
	p := cmdc("cd& echo %GP_TEST%")
	p.Cwd, p.Env = t.TempDir(), map[string]string{"gp_test": "v1"} // names are case-insensitive
	pid := c.run(t, m, p)
	out, _, _, _ := c.forPid(pid)
	lines := strings.Fields(string(out))
	if len(lines) != 2 || !strings.EqualFold(lines[0], p.Cwd) || lines[1] != "v1" {
		t.Fatalf("stdout = %q, want cwd %q", out, p.Cwd)
	}
}

func TestWindowsStdin(t *testing.T) {
	m, c := newManager()
	res, err := m.Start(StartParams{Argv: []string{"findstr.exe", "x*"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)
	m.Stdin(StdinParams{Pid: res.Pid, Data: []byte("hello ")})
	m.Stdin(StdinParams{Pid: res.Pid, Data: []byte("world\r\n"), EOF: true})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("findstr never exited")
	}
	if out, _, _, _ := c.forPid(res.Pid); strings.TrimSpace(string(out)) != "hello world" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestWindowsKillTakesTheWholeTree(t *testing.T) {
	m, c := newManager()
	res, err := m.Start(cmdc("ping -n 60 127.0.0.1 >nul"))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)

	var grandchild uint32
	for deadline := time.Now().Add(10 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		grandchild = childOf(uint32(res.Pid))
	}
	if grandchild == 0 {
		t.Fatal("cmd never started its child")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, grandchild)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	if _, err := m.Kill(KillParams{Pid: res.Pid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no exec.exit after kill")
	}
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		t.Fatal("grandchild survived the kill")
	}
}

// As on Linux: a start whose reply never reached the host ends the whole tree.
func TestWindowsUnansweredStartEndsTheTree(t *testing.T) {
	m, c := newManager()
	res, err := m.Start(cmdc("ping -n 60 127.0.0.1 >nul"))
	if err != nil {
		t.Fatal(err)
	}
	var grandchild uint32
	for deadline := time.Now().Add(10 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		grandchild = childOf(uint32(res.Pid))
	}
	if grandchild == 0 {
		t.Fatal("cmd never started its child")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, grandchild)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	res.Sent(false)
	if _, err := m.Kill(KillParams{Pid: res.Pid}); errCode(err) != proto.CodeNotFound {
		t.Fatalf("kill after an unanswered start: %v", err)
	}
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		t.Fatal("grandchild survived")
	}
	time.Sleep(200 * time.Millisecond)
	if _, _, methods, _ := c.forPid(res.Pid); len(methods) != 0 {
		t.Fatalf("notifications for a pid the host never saw: %v", methods)
	}
}

func childOf(parent uint32) uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID == parent {
			return pe.ProcessID
		}
	}
	return 0
}

func TestWindowsStartErrors(t *testing.T) {
	m, _ := newManager()
	if _, err := m.Start(StartParams{Argv: []string{`C:\nonexistent\prog.exe`}}); errCode(err) != proto.CodeNotFound {
		t.Errorf("missing program: %v", err)
	}
	if _, err := m.Start(StartParams{}); errCode(err) != proto.CodeInvalidParams {
		t.Errorf("empty argv: %v", err)
	}
}

// With nobody signed in on the console as:user must fail with code 4; with a
// user signed in (run as LocalSystem, since only it may query the user token)
// the process must run as that user.
func TestWindowsAsUser(t *testing.T) {
	m, c := newManager()
	tok, err := ConsoleUserToken()
	if err == nil {
		tok.Close()
	} else if errCode(err) != proto.CodeNoUser {
		t.Skipf("cannot query the console user token (not LocalSystem?): %v", err)
	}
	p := cmdc("echo %USERNAME%& cd")
	p.As = "user"
	if err != nil {
		if _, serr := m.Start(p); errCode(serr) != proto.CodeNoUser {
			t.Fatalf("no user signed in: Start = %v, want code 4", serr)
		}
		t.Log("verified: code 4 with no signed-in user")
		return
	}
	pid := c.run(t, m, p)
	out, _, _, code := c.forPid(pid)
	t.Logf("as user ran: exit=%d stdout=%q", code, out)
	if code != 0 || len(strings.Fields(string(out))) != 2 {
		t.Fatalf("exit=%d stdout=%q", code, out)
	}
}
