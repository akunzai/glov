package broker

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// PipeSDDL is the broker pipe's security descriptor: owned by and fully
// accessible to LocalSystem, read/write for authenticated users without
// FILE_CREATE_PIPE_INSTANCE (0x12019b), so nobody else can add server
// instances.
const PipeSDDL = "O:SYD:P(A;;GA;;;SY)(A;;0x12019b;;;AU)"

// HCSSystems is Systems on the Host Compute Service.
type HCSSystems struct{}

func (HCSSystems) Create(id string, vm hcs.LinuxVM) (System, error) {
	sys, err := hcs.Create(id, vm.Document())
	if err != nil {
		return nil, err
	}
	return hcsSystem{sys}, nil
}

func (HCSSystems) Open(id string) (System, error) {
	sys, err := hcs.Open(id)
	if err != nil {
		return nil, err
	}
	return hcsSystem{sys}, nil
}

type hcsSystem struct{ *hcs.System }

func (s hcsSystem) Stats() (Stats, error) {
	doc, err := s.Properties(`{"PropertyTypes":["Statistics"]}`)
	if err != nil {
		return Stats{}, err
	}
	var p struct {
		Statistics struct {
			Processor struct{ TotalRuntime100ns uint64 }
		}
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return Stats{}, fmt.Errorf("statistics %q: %w", doc, err)
	}
	return Stats{CPUTime100ns: p.Statistics.Processor.TotalRuntime100ns}, nil
}

// Listen creates the broker's pipe.
func Listen() (net.Listener, error) {
	return winio.ListenPipe(PipeName, &winio.PipeConfig{SecurityDescriptor: PipeSDDL})
}

// Serve answers connections on ln until ctx ends or ln fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, winio.ErrPipeListenerClosed) || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
		wg.Go(func() { s.serveConn(ctx, c) })
	}
}

func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	sid, session, err := winsec.PipeClient(c)
	if err != nil {
		slog.Warn("connection refused: caller unknown", "err", err)
		return
	}
	root, err := VMRoot(sid)
	if err != nil {
		slog.Warn("connection refused", "user", sid, "err", err)
		return
	}
	methods, end := s.Methods(Caller{SID: sid, Session: session, OpenDisk: func(path string) (func(), error) {
		return openDisk(c, sid, root, path)
	}})
	defer end()
	conn := proto.NewConn(c)
	if err := methods.Serve(ctx, conn); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		slog.Debug("connection ended", "user", sid, "err", err)
	}
}

// VMRoot is where user sid's VM disks may lie: the vms folder of their
// glov home, which is the GLOV_HOME user environment variable if set, or
// else glov under their local application data folder.
func VMRoot(sid string) (string, error) {
	home, err := userHome(sid)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "vms"), nil
}

func userHome(sid string) (string, error) {
	if k, err := registry.OpenKey(registry.USERS, sid+`\Environment`, registry.QUERY_VALUE); err == nil {
		v, _, err := k.GetStringValue("GLOV_HOME")
		k.Close()
		if err == nil && v != "" {
			if strings.Contains(v, "%") || !filepath.IsAbs(v) {
				return "", fmt.Errorf("GLOV_HOME %q: want an absolute path without variables", v)
			}
			return filepath.Clean(v), nil
		}
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("profile of %s: %w", sid, err)
	}
	defer k.Close()
	profile, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return "", fmt.Errorf("profile of %s: %w", sid, err)
	}
	return filepath.Join(profile, `AppData\Local\glov`), nil
}

// openDisk checks, as the pipe client c (user sid), that path is a virtual
// disk under root that sid owns. It opens every folder from the volume
// root down and the file itself without following reparse points and
// without FILE_SHARE_DELETE, refusing any reparse point, so none can be
// renamed or replaced until release closes them.
func openDisk(c net.Conn, sid, root, path string) (release func(), err error) {
	if err := diskPath(root, path); err != nil {
		return nil, err
	}
	var pins []windows.Handle
	release = func() {
		for _, h := range pins {
			windows.CloseHandle(h)
		}
	}
	err = winsec.AsPipeClient(c, func() error {
		parts := strings.Split(path, `\`)
		cur := parts[0] + `\`
		for i := 1; i < len(parts); i++ {
			cur = filepath.Join(cur, parts[i])
			last := i == len(parts)-1
			access := uint32(windows.READ_CONTROL | windows.FILE_LIST_DIRECTORY)
			if last {
				access = windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES
			}
			p, err := windows.UTF16PtrFromString(cur)
			if err != nil {
				return err
			}
			h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				return fmt.Errorf("disk %s: open %s: %w", path, cur, err)
			}
			pins = append(pins, h)
			var fi windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
				return err
			}
			if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("disk %s: %s is a link or junction", path, cur)
			}
			dir := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
			if dir == last {
				return fmt.Errorf("disk %s: %s is not a %s", path, cur, map[bool]string{true: "file", false: "folder"}[last])
			}
			if last {
				if fi.NumberOfLinks != 1 {
					return fmt.Errorf("disk %s: %d hard links", path, fi.NumberOfLinks)
				}
				sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
				if err != nil {
					return err
				}
				owner, _, err := sd.Owner()
				if err != nil {
					return err
				}
				if owner.String() != sid {
					return fmt.Errorf("disk %s: owned by %s, not you", path, owner)
				}
			}
		}
		return nil
	})
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// diskPath checks path's shape: a clean absolute path on a drive letter,
// under root, naming a .vhdx or .vhd.
func diskPath(root, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(filepath.VolumeName(path)) != 2 {
		return fmt.Errorf("disk %s: want a clean absolute path on a local drive", path)
	}
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)+`\`) {
		return fmt.Errorf("disk %s: not under %s", path, root)
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext != ".vhdx" && ext != ".vhd" {
		return fmt.Errorf("disk %s: not a .vhdx or .vhd", path)
	}
	return nil
}

// SignedIn reports whether the user sid is signed in to a Windows session
// other than except, connected or not.
func SignedIn(sid string, except uint32) bool {
	var sessions *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &n); err != nil {
		slog.Warn("listing Windows sessions", "err", err)
		return false
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	for _, s := range unsafe.Slice(sessions, n) {
		if s.SessionID == except {
			continue
		}
		var t windows.Token
		if windows.WTSQueryUserToken(s.SessionID, &t) != nil {
			continue // no user in it
		}
		u, err := t.GetTokenUser()
		t.Close()
		if err == nil && u.User.Sid.String() == sid {
			return true
		}
	}
	return false
}
