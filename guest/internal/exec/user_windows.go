package exec

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/guest/internal/winsys"
	"github.com/akunzai/glov/guest/proto"
)

// userChild is a process created with CreateProcessAsUser in the console
// user's session.
type userChild struct {
	pid            int
	process, job   windows.Handle
	stdout, stderr *os.File
}

func (c *userChild) Pid() int          { return c.pid }
func (c *userChild) Stdout() io.Reader { return c.stdout }
func (c *userChild) Stderr() io.Reader { return c.stderr }

func (c *userChild) Wait() int {
	windows.WaitForSingleObject(c.process, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(c.process, &code)
	return int(code)
}

func (c *userChild) Kill() error {
	if c.job != 0 {
		return windows.TerminateJobObject(c.job, 1)
	}
	return windows.TerminateProcess(c.process, 1)
}

func (c *userChild) Close() {
	c.stdout.Close()
	c.stderr.Close()
	windows.CloseHandle(c.process)
	if c.job != 0 {
		windows.CloseHandle(c.job)
	}
}

// ConsoleUserToken returns a primary token for the user signed in on the
// console, or an error with code 4 when nobody is.
func ConsoleUserToken() (windows.Token, error) {
	noUser := proto.Errorf(proto.CodeNoUser, "no user is signed in on the console")
	sid := windows.WTSGetActiveConsoleSessionId()
	if sid == 0xFFFFFFFF {
		return 0, noUser
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(sid, &tok); err != nil {
		if errors.Is(err, windows.ERROR_NO_TOKEN) {
			return 0, noUser
		}
		return 0, err
	}
	return tok, nil
}

func startUser(p StartParams) (child, io.WriteCloser, error) {
	// LocalSystem holds these but may have them disabled.
	winsys.EnablePrivilege("SeAssignPrimaryTokenPrivilege")
	winsys.EnablePrivilege("SeIncreaseQuotaPrivilege")

	tok, err := ConsoleUserToken()
	if err != nil {
		return nil, nil, err
	}
	defer tok.Close()

	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, tok, false); err != nil {
		return nil, nil, err
	}
	env := mergeEnv(blockToEnv(block), p.Env, true)
	windows.DestroyEnvironmentBlock(block)

	dir := p.Cwd
	if dir == "" {
		if dir, err = tok.GetUserProfileDirectory(); err != nil {
			return nil, nil, err
		}
	}
	exe, err := lookPath(p.Argv[0], env)
	if err != nil {
		return nil, nil, err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return nil, nil, err
	}
	cmdPtr, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(p.Argv))
	if err != nil {
		return nil, nil, err
	}
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, nil, err
	}
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	envBlock := envToBlock(env)

	inR, inW, err := newPipe(false)
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := newPipe(true)
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := newPipe(true)
	if err != nil {
		return nil, nil, err
	}
	childEnds := []windows.Handle{inR, outW, errW}
	closeChildEnds := func() {
		for _, h := range childEnds {
			windows.CloseHandle(h)
		}
	}
	fail := func(err error) (child, io.WriteCloser, error) {
		closeChildEnds()
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		windows.CloseHandle(errR)
		return nil, nil, err
	}

	si := windows.StartupInfo{
		Desktop:   desktop,
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  inR,
		StdOutput: outW,
		StdErr:    errW,
	}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	// shortcut: inheritHandles=true passes every inheritable handle of the
	// Guest Service; the only ones are the three pipe ends above and starts are
	// serialised by the request loop.
	if err := windows.CreateProcessAsUser(tok, exePtr, cmdPtr, nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, &envBlock[0], dirPtr, &si, &pi); err != nil {
		return fail(err)
	}
	closeChildEnds()
	windows.CloseHandle(pi.Thread)

	c := &userChild{
		pid:     int(pi.ProcessId),
		process: pi.Process,
		stdout:  os.NewFile(uintptr(outR), "stdout"),
		stderr:  os.NewFile(uintptr(errR), "stderr"),
	}
	if job, err := assignToNewJob(pi.Process); err == nil {
		c.job = job
	}
	if p.Stdin {
		return c, os.NewFile(uintptr(inW), "stdin"), nil
	}
	windows.CloseHandle(inW) // the child sees end-of-file at once
	return c, nil, nil
}

// newPipe makes a pipe whose child end is inheritable. The parent end is the
// write end when parentReads is false.
func newPipe(parentReads bool) (r, w windows.Handle, err error) {
	sa := windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err = windows.CreatePipe(&r, &w, &sa, 0); err != nil {
		return
	}
	parent := w
	if parentReads {
		parent = r
	}
	if err = windows.SetHandleInformation(parent, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(r)
		windows.CloseHandle(w)
	}
	return
}

func blockToEnv(block *uint16) []string {
	var env []string
	for p := unsafe.Pointer(block); ; {
		n := 0
		for *(*uint16)(unsafe.Add(p, 2*n)) != 0 {
			n++
		}
		if n == 0 {
			return env
		}
		env = append(env, windows.UTF16ToString(unsafe.Slice((*uint16)(p), n)))
		p = unsafe.Add(p, 2*(n+1))
	}
}

// envToBlock builds a double-NUL-terminated block, sorted by name as
// CreateProcess documents.
func envToBlock(env []string) []uint16 {
	env = slices.Clone(env)
	slices.SortFunc(env, func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) })
	var block []uint16
	for _, kv := range env {
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	return append(block, 0, 0)
}

// lookPath resolves name like CreateProcess would, but against the target
// user's PATH and PATHEXT rather than the Guest Service's.
func lookPath(name string, env []string) (string, error) {
	get := func(key string) string {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
				return v
			}
		}
		return ""
	}
	exts := strings.Split(get("PATHEXT"), ";")
	if exts[0] == "" {
		exts = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	try := func(base string) string {
		if filepath.Ext(base) != "" {
			if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
				return base
			}
		}
		for _, e := range exts {
			if fi, err := os.Stat(base + e); err == nil && !fi.IsDir() {
				return base + e
			}
		}
		return ""
	}
	if strings.ContainsAny(name, `\/:`) {
		if f := try(name); f != "" {
			return f, nil
		}
	} else {
		for _, d := range filepath.SplitList(get("PATH")) {
			if d == "" {
				continue
			}
			if f := try(filepath.Join(d, name)); f != "" {
				return f, nil
			}
		}
	}
	return "", proto.Errorf(proto.CodeNotFound, "executable not found: %s", name)
}
