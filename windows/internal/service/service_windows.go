// Package service installs and removes glov on a Windows host: each user's
// glovd, which runs as that user from their glov home (glov host install,
// no elevation), and glov-broker, the one LocalSystem service that makes
// the Host Compute Service calls a user cannot (glov host install-broker,
// elevated, once).
package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/daemon"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// GuestBinary is the Guest Service built for the VMs' Linux (x86_64);
// glovd seeds new VMs with it.
const GuestBinary = "glov-guest-linux-amd64"

// Binaries are what glov host install copies from the directory glov runs
// from into BinDir.
var Binaries = []string{"glov.exe", "glovd.exe", GuestBinary}

// Home is the user's glov home: VM disks and records, the image cache,
// secrets, glovd's log and BinDir. GLOV_HOME moves it; glov-broker only
// boots disks under it when GLOV_HOME is a user environment variable.
func Home() string {
	if h := os.Getenv("GLOV_HOME"); h != "" {
		return h
	}
	local, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		local = os.Getenv("LOCALAPPDATA")
	}
	return filepath.Join(local, "glov")
}

// checkHome refuses a GLOV_HOME that glovd at sign-in and glov-broker
// would not see: both read the one saved in the user's environment, not
// this shell's.
func checkHome() error {
	saved := ""
	if k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE); err == nil {
		saved, _, _ = k.GetStringValue("GLOV_HOME")
		k.Close()
	}
	if saved != "" && (strings.Contains(saved, "%") || !filepath.IsAbs(saved)) {
		return fmt.Errorf("GLOV_HOME in your user environment is %q: glov-broker takes only an absolute path without %%VARIABLES%%", saved)
	}
	if shell := os.Getenv("GLOV_HOME"); (shell != "" || saved != "") && !samePath(shell, saved) {
		return fmt.Errorf("GLOV_HOME is %q in this shell but %q in your user environment, which glovd at sign-in and glov-broker read: set it there (setx GLOV_HOME <dir>, or remove it) and open a new shell", shell, saved)
	}
	return nil
}

// BinDir holds the installed glov, glovd and Guest Service.
func BinDir() string { return filepath.Join(Home(), "bin") }

const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "glovd"
)

// Install copies glov and glovd from srcDir to BinDir, prepares the glov
// home, adds BinDir to the user's PATH, has Windows start glovd at sign-in
// and starts it. Run again, it upgrades in place: glovd restarts and the
// VMs keep running.
//
// cache, if not empty, is an absolute local path for the image cache instead
// of <home>\cache. Empty on an upgrade keeps the cache directory the
// previous install set.
func Install(srcDir, cache string, out io.Writer) error {
	if winsec.Elevated() {
		return errors.New("run glov host install from a non-elevated shell: glovd runs as you, and the files an elevated glovd made would belong to Administrators, which glov-broker refuses to boot")
	}
	me, err := winsec.CurrentSID()
	if err != nil {
		return err
	}
	if err := checkHome(); err != nil {
		return err
	}
	if cache != "" {
		if err := checkCachePath(cache); err != nil {
			return err
		}
	}
	prev, _ := runCommand()
	if cache == "" {
		cache = cacheFlag(prev)
	}
	if err := checkVMsInCache(Home(), cmp.Or(cache, defaultCache())); err != nil {
		return err
	}
	if err := winsec.CreateDataDir(Home(), me); err != nil {
		return err
	}
	if cache != "" {
		if err := winsec.CreateDataDir(cache, me); err != nil {
			return fmt.Errorf("image cache: %w", err)
		}
	}
	if err := quitGlovd(me); err != nil {
		return err
	}
	if err := copyBinaries(srcDir, BinDir(), Binaries); err != nil {
		return err
	}
	exe := filepath.Join(BinDir(), "glovd.exe")
	if err := setRunCommand(commandLine(exe, cache)); err != nil {
		return err
	}
	added, err := addToPath(BinDir())
	if err != nil {
		return err
	}
	if _, err := StartGlovd(); err != nil {
		return err
	}
	if err := waitGlovd(me, 30*time.Second); err != nil {
		return fmt.Errorf("%w; see %s", err, filepath.Join(Home(), "glovd.log"))
	}
	fmt.Fprintf(out, "glovd installed in %s and running; data in %s, images in %s\n", BinDir(), Home(), cmp.Or(cache, defaultCache()))
	if added {
		fmt.Fprintf(out, "added %s to your PATH: open a new terminal to use glov from it\n", BinDir())
	}
	brokerNote(out)
	return nil
}

// brokerNote tells the user what is missing before VMs can start.
func brokerNote(out io.Writer) {
	if serviceExists(LegacyName) {
		fmt.Fprintln(out, "note: the old LocalSystem glovd service is still installed; `glov host install-broker` (elevated) removes it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := broker.Dial(ctx)
	if err != nil {
		fmt.Fprintf(out, "note: VMs cannot start yet: %v\n", err)
		return
	}
	c.Close()
}

func defaultCache() string { return filepath.Join(Home(), "cache") }

// Uninstall stops glovd, keeps Windows from starting it at sign-in, and
// removes BinDir and its PATH entry. Running VMs keep running, and the
// glov home, with the VMs and data, stays.
func Uninstall(out io.Writer) error {
	me, err := winsec.CurrentSID()
	if err != nil {
		return err
	}
	if err := quitGlovd(me); err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err == nil {
		err = k.DeleteValue(runValue)
		k.Close()
	}
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("remove glovd from sign-in: %w", err)
	}
	if err := removeFromPath(BinDir()); err != nil {
		return err
	}
	left, err := removeBinDir(BinDir())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "glovd removed; VMs and data stay in %s\n", Home())
	if left {
		fmt.Fprintf(out, "%s still holds this glov.exe, renamed; delete it once glov has exited\n", BinDir())
	}
	return nil
}

// StartGlovd starts the installed glovd as it starts at sign-in, detached
// from this console. The channel receives glovd's exit, should it exit
// while this process runs.
func StartGlovd() (<-chan error, error) {
	line, err := runCommand()
	if err != nil {
		return nil, errors.New("glovd is not installed for you: run glov host install")
	}
	cmd, err := glovdCommand(line)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start glovd: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// glovdCommand is glovd started from its sign-in command line, detached and
// in its own folder: a working directory taken from the shell would stay in
// use for as long as glovd runs, so the folder glov ran from (an unpacked
// release, a project) could not be deleted or renamed.
func glovdCommand(line string) (*exec.Cmd, error) {
	args, err := windows.DecomposeCommandLine(line)
	if err != nil || len(args) == 0 {
		return nil, fmt.Errorf("glovd's sign-in command %q: %v", line, err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = filepath.Dir(args[0])
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	return cmd, nil
}

// StartupError says why glovd refused to start, when the reason is one of
// the checks it runs on the glov home and image cache before it opens
// glovd.log; nil otherwise.
func StartupError(me string) error {
	line, _ := runCommand()
	for _, dir := range []string{Home(), cmp.Or(cacheFlag(line), defaultCache())} {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := winsec.CheckDataDir(dir, me); err != nil {
			return err
		}
	}
	return nil
}

// quitGlovd asks the user's running glovd to exit and waits until it has.
func quitGlovd(me string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := daemon.Dial(ctx, api.Pipe(me), me, nil)
	if errors.Is(err, daemon.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	err = c.Call(ctx, api.MethodQuit, nil, nil)
	c.Close()
	// glovd may close the connection before its reply goes out; the probe
	// below confirms it stopped.
	if err != nil && !errors.Is(err, rpc.ErrClosed) {
		return fmt.Errorf("stop glovd: %w", err)
	}
	for {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		c, err := daemon.Dial(probe, api.Pipe(me), me, nil)
		cancel()
		if errors.Is(err, daemon.ErrNotRunning) {
			return nil
		}
		if c != nil {
			c.Close()
		}
		if ctx.Err() != nil {
			return errors.New("glovd did not stop within 30 s")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitGlovd waits until the user's glovd answers.
func waitGlovd(me string, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		probe, pcancel := context.WithTimeout(ctx, time.Second)
		c, err := daemon.Dial(probe, api.Pipe(me), me, nil)
		pcancel()
		if err == nil {
			c.Close()
			return nil
		}
		if !errors.Is(err, daemon.ErrNotRunning) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("glovd did not answer within %s", d)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runCommand() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return v, err
}

func setRunCommand(line string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("start glovd at sign-in: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(runValue, line); err != nil {
		return fmt.Errorf("start glovd at sign-in: %w", err)
	}
	return nil
}

// checkVMsInCache refuses a cache directory that would leave VMs behind:
// each VM's differencing disk names its base image by full path, so a VM
// whose base image lies outside cache would break once the old directory
// is deleted.
func checkVMsInCache(dataDir, cache string) error {
	records, err := store.Store{Root: dataDir}.Records()
	if err != nil {
		return err
	}
	var left []string
	for _, r := range records {
		if !inDir(r.BaseDisk, cache) {
			left = append(left, fmt.Sprintf("%s (%s)", r.Name, r.BaseDisk))
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the image cache cannot move to %s: these VMs use base images elsewhere: %s. Delete them first, or install with their --cache",
			cache, strings.Join(left, ", "))
	}
	return nil
}

// inDir reports whether path lies inside dir, ignoring case as NTFS does.
func inDir(path, dir string) bool {
	rel, err := filepath.Rel(strings.ToLower(filepath.Clean(dir)), strings.ToLower(filepath.Clean(path)))
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, `..\`)
}

// checkCachePath accepts an absolute path on a local drive letter: HCS opens
// VM disks by path, and each VM's disk names its base image in the cache.
func checkCachePath(dir string) error {
	if !filepath.IsAbs(dir) || len(filepath.VolumeName(dir)) != 2 {
		return fmt.Errorf(`--cache %s: want an absolute path on a local drive, such as D:\glov\cache`, dir)
	}
	return nil
}

func args(cache string) []string {
	if cache == "" {
		return nil
	}
	return []string{"--cache", cache}
}

// commandLine quotes exe and its arguments for the Run key.
func commandLine(exe, cache string) string {
	s := windows.EscapeArg(exe)
	for _, a := range args(cache) {
		s += " " + windows.EscapeArg(a)
	}
	return s
}

// cacheFlag returns the --cache argument of an installed command line.
func cacheFlag(cmdLine string) string {
	if cmdLine == "" {
		return ""
	}
	a, err := windows.DecomposeCommandLine(cmdLine)
	if err != nil {
		return ""
	}
	for i, s := range a {
		if s == "--cache" && i+1 < len(a) {
			return a[i+1]
		}
	}
	return ""
}

// addToPath appends dir to the user's PATH unless it is there already, and
// tells running programs (Explorer, which new terminals start from).
func addToPath(dir string) (added bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("your PATH: %w", err)
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return false, fmt.Errorf("your PATH: %w", err)
	}
	if pathHas(cur, dir) {
		return false, nil
	}
	next := dir
	if strings.TrimSpace(cur) != "" {
		next = strings.TrimSuffix(cur, ";") + ";" + dir
	}
	if typ == registry.SZ {
		err = k.SetStringValue("Path", next)
	} else {
		err = k.SetExpandStringValue("Path", next)
	}
	if err != nil {
		return false, fmt.Errorf("your PATH: %w", err)
	}
	broadcastEnvironment()
	return true, nil
}

// removeFromPath takes dir out of the user's PATH.
func removeFromPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil || !pathHas(cur, dir) {
		return nil
	}
	var keep []string
	for _, e := range strings.Split(cur, ";") {
		if e != "" && !samePath(e, dir) {
			keep = append(keep, e)
		}
	}
	next := strings.Join(keep, ";")
	if typ == registry.SZ {
		err = k.SetStringValue("Path", next)
	} else {
		err = k.SetExpandStringValue("Path", next)
	}
	if err != nil {
		return fmt.Errorf("your PATH: %w", err)
	}
	broadcastEnvironment()
	return nil
}

func pathHas(list, dir string) bool {
	for _, e := range strings.Split(list, ";") {
		if samePath(e, dir) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	clean := func(p string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(p), `\`)) }
	return clean(a) != "" && clean(a) == clean(b)
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

// broadcastEnvironment tells top-level windows the environment changed, as
// the System Properties dialog does, so programs started afterwards see
// the new PATH.
func broadcastEnvironment() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	env, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	procSendMessageTimeoutW.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}

// copyBinaries copies names from srcDir to dstDir. A running copy cannot
// be overwritten but can be renamed away: earlier copies still running (a
// live share's agent outlives upgrades) keep theirs, each under a name of
// its own, removed by a later install once nothing runs it.
func copyBinaries(srcDir, dstDir string, names []string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, b := range names {
		src, dst := filepath.Join(srcDir, b), filepath.Join(dstDir, b)
		if same, err := sameFile(src, dst); err != nil || same {
			if err != nil {
				return fmt.Errorf("%s: %w (%s must sit in the same directory)", src, err, strings.Join(names, ", "))
			}
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if olds, _ := filepath.Glob(dst + ".old*"); len(olds) > 0 {
			for _, o := range olds {
				os.Remove(o)
			}
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			old := dst + ".old"
			if _, serr := os.Stat(old); serr == nil {
				old = fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
			}
			if rerr := os.Rename(dst, old); rerr != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, nil
	}
	return os.SameFile(fa, fb), nil
}

// removeBinDir deletes dir; a file still in use (this glov.exe) is renamed
// and left, which left reports.
func removeBinDir(dir string) (left bool, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if os.RemoveAll(p) == nil {
			continue
		}
		if strings.Contains(e.Name(), ".old") {
			left = true
			continue
		}
		if err := os.Rename(p, fmt.Sprintf("%s.old-%d", p, time.Now().UnixNano())); err != nil {
			return false, fmt.Errorf("remove %s: %w", p, err)
		}
		left = true
	}
	if !left {
		return false, os.Remove(dir)
	}
	return true, nil
}
