// Package api is the contract between glov and glovd on Windows: method
// names, parameters and results, sent as framed JSON-RPC 2.0 over the
// \\.\pipe\glovd named pipe. JSON field names follow the macOS daemon's
// results (Sources/GlovDaemonAPI/RPC.swift), so scripts and agents read
// both editions' status the same way.
package api

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/akunzai/glov/windows/internal/share"
)

// Pipe is the control pipe of user sid's glovd. The user's glovd owns
// it, which glov checks: another user cannot create a pipe owned by them.
func Pipe(sid string) string { return `\\.\pipe\glovd-` + sid }

// A connection to Pipe that starts with SSHHeader, followed by a VM name
// and a newline (at most SSHHeaderMax bytes in all), is an SSH relay for
// glov ssh rather than JSON-RPC: glovd answers "ok\n" or
// "error: <reason>\n", then pipes the connection to the VM's sshd
// (docs/guest-protocol.md "SSH over vsock").
const (
	SSHHeader    = "glov-ssh "
	SSHHeaderMax = 512
)

// ProtocolVersion changes when an older glov or glovd would misread a message.
const ProtocolVersion = 1

// Methods.
const (
	MethodInfo = "daemon.info"
	// MethodQuit stops glovd, leaving the VMs running (glov host install
	// and uninstall).
	MethodQuit     = "daemon.quit"
	MethodVMList   = "vm.list"
	MethodVMStatus = "vm.status"
	MethodVMCreate = "vm.create"
	MethodVMStart  = "vm.start"
	MethodVMStop   = "vm.stop"
	MethodVMReboot = "vm.reboot"
	MethodVMWait   = "vm.wait"
	// MethodVMReset returns a stopped, frozen VM to its template;
	// MethodVMFreeze (glov vm harness install) makes a stopped isolated VM's
	// disk its template.
	MethodVMReset  = "vm.reset"
	MethodVMFreeze = "vm.freeze"
	// MethodVMHarnessAdd (glov vm harness add) sets a running isolated
	// VM's guest up for one more harness, as provisioning does for those
	// given to vm create, allows its runtime hosts and records it; glov
	// then installs it.
	MethodVMHarnessAdd = "vm.harnessAdd"
	// MethodVMConfigure changes a stopped VM; MethodVMDelete removes one.
	MethodVMConfigure = "vm.configure"
	MethodVMDelete    = "vm.delete"
	// MethodVMConsole reads the serial console log.
	MethodVMConsole = "vm.console"
	// Guest commands: glov exec starts a job and reads its output until it
	// exits; glov cp moves file contents with GuestCall's file methods,
	// reading and writing host files itself, as the user.
	MethodExecStart = "exec.start"
	MethodExecRead  = "exec.read"
	MethodExecKill  = "exec.kill"
	MethodGuestCall = "guest.call"
	// Shares: glov resolves and checks the host folder as the user, then
	// glovd records it in vm.json. glovd never reads a share.
	MethodShareAdd    = "share.add"
	MethodShareRemove = "share.remove"
	MethodShareList   = "share.list"
	// MethodGuestCheck compares the VM's Guest Service with glovd's by
	// SHA-256; MethodGuestUpdate installs glovd's on the running VM.
	MethodGuestCheck  = "guest.check"
	MethodGuestUpdate = "guest.update"
	// MethodVMStats measures the host and the running VMs.
	MethodVMStats = "vm.stats"
	// Egress methods act on an isolated VM's egress policy and log.
	MethodEgressList   = "egress.list"
	MethodEgressAllow  = "egress.allow"
	MethodEgressRemove = "egress.remove"
	MethodEgressLog    = "egress.log"
	MethodEgressMCP    = "egress.mcp"
	// MethodEgressMode opens the VM to any public host, through the proxy,
	// or restricts it to its allowlist again. glov mcp relays no tool for it.
	MethodEgressMode = "egress.mode"
	// Trust methods keep the CA certificates an isolated VM trusts, in its
	// system store and in its proxy's upstream checks. No MCP tool.
	MethodTrustList   = "trust.list"
	MethodTrustAdd    = "trust.add"
	MethodTrustRemove = "trust.remove"

	// Secret methods keep the secrets isolated VMs' egress proxies put in
	// place of placeholders, and the VMs' grants. None returns a value; a
	// command secret's command runs in glov, as the user, which sends what
	// it printed with secret.source or secret.refresh.
	MethodSecretSet     = "secret.set"
	MethodSecretSource  = "secret.source"
	MethodSecretRefresh = "secret.refresh"
	MethodSecretList    = "secret.list"
	MethodSecretRemove  = "secret.remove"
	MethodSecretGrant   = "secret.grant"
	MethodSecretRevoke  = "secret.revoke"
	MethodSecretGrants  = "secret.grants"
	MethodSecretRepos   = "secret.repos"
	MethodSecretLogin   = "secret.login"
	MethodSecretLogout  = "secret.logout"
)

// CodePlainSecrets is egress.mode's and secret.grant's refusal to let an
// open VM hold tmpfs secrets as real values, until the call is repeated
// with AllowPlainSecrets.
const CodePlainSecrets = -32002

// NotifyProgress is a notification glovd sends while a request runs
// long (vm.create downloading and converting the image); params are Progress.
const NotifyProgress = "progress"

// Progress is one line to show the user.
type Progress struct {
	Message string `json:"message"`
}

// vm.create defaults and limits.
const (
	DefaultCPUs      = 2
	DefaultMemoryMiB = 4096
	DefaultDiskGiB   = 32
	MinMemoryMiB     = 256
	// MaxDiskGiB is the VHDX format's limit, 64 TiB. A disk takes host space
	// only as the guest writes to it.
	MaxDiskGiB = 64 << 10
)

// Linux distributions a VM can run (decision 1 in akunzai/glov#146).
const (
	DistroDebian = "debian"
	DistroUbuntu = "ubuntu"
)

// Distros lists them, the default first.
var Distros = []string{DistroUbuntu, DistroDebian}

// SecureBoot reports whether distro's VMs boot with Secure Boot: Debian's
// and Ubuntu's shim, GRUB and kernel are signed through Microsoft's UEFI CA.
func SecureBoot(distro string) bool {
	return distro == DistroDebian || distro == DistroUbuntu
}

// CreateParams are vm.create's; zero means the default.
type CreateParams struct {
	VM        string `json:"vm"`
	CPUs      int    `json:"cpus,omitempty"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
	DiskGiB   int    `json:"diskGiB,omitempty"`
	// Distro is one of Distros.
	Distro string `json:"distro,omitempty"`
	// Isolated creates a VM with no network adapter, whose only way out is
	// the egress proxy (Debian and Ubuntu).
	Isolated bool `json:"isolated,omitzero"`
	// Harnesses (isolated only) are the coding-agent harness profiles to
	// set up: their settings, runtime hosts and model API.
	Harnesses []string `json:"harnesses,omitempty"`
	// Open (isolated only) creates the VM in egress mode open.
	Open bool `json:"open,omitzero"`
	// Project (isolated only) makes it the VM of a project (glov claude).
	Project *Project `json:"project,omitempty"`
}

// Project is what a project's VM (glov claude, codex or agy) belongs to:
// the project's folder on the host, and how its files reach the guest.
type Project struct {
	Path string `json:"path"`
	// Sync is SyncGit or SyncLive.
	Sync string `json:"sync"`
}

// How a project reaches its VM: SyncGit pushes commits to a repository on
// the guest's disk and fetches them back; SyncLive shares the working
// tree, .git included, both ways.
const (
	SyncGit  = "git"
	SyncLive = "live"
)

// Info is the daemon.info result.
type Info struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Version         string `json:"version"`
}

// VMParams names one VM by name or unique prefix.
type VMParams struct {
	VM string `json:"vm"`
}

// HarnessAddParams are vm.harnessAdd's.
type HarnessAddParams struct {
	VM      string `json:"vm"`
	Harness string `json:"harness"`
}

// HarnessAdded is vm.harnessAdd's result: the hosts it allowed, the home
// files it kept because they were there already, and the tmpfs secrets an
// open VM withholds from it without --allow-plain-secrets. Already says
// the VM had the harness, and nothing changed.
type HarnessAdded struct {
	VM       string   `json:"vm"`
	Harness  string   `json:"harness"`
	Already  bool     `json:"already,omitzero"`
	Allowed  []string `json:"allowed,omitempty"`
	Kept     []string `json:"kept,omitempty"`
	Withheld []string `json:"withheld,omitempty"`
}

// ConfigureParams are vm.configure's; zero leaves a setting as it is.
type ConfigureParams struct {
	VM        string `json:"vm"`
	CPUs      int    `json:"cpus,omitempty"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
	// DiskGiB only grows the disk.
	DiskGiB int `json:"diskGiB,omitempty"`
}

// ExecParams are exec.start's.
type ExecParams struct {
	VM   string            `json:"vm"`
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// As is "system" (the default, root) or "user".
	As string `json:"as,omitempty"`
}

// JobStarted is exec.start's result.
type JobStarted struct {
	JobID string `json:"jobId"`
}

// ExecReadParams are exec.read's.
type ExecReadParams struct {
	VM    string `json:"vm"`
	JobID string `json:"jobId"`
	// Cursor is the previous read's cursor, 0 at first.
	Cursor   int `json:"cursor,omitempty"`
	MaxBytes int `json:"maxBytes,omitempty"`
}

// MaxReadBytes caps exec.read's maxBytes, and is its default.
const MaxReadBytes = 256 << 10

// ExecRead is exec.read's result: stdout and stderr interleaved.
type ExecRead struct {
	Data   []byte `json:"data"`
	Cursor int    `json:"cursor"`
	Exited bool   `json:"exited"`
	Code   *int   `json:"code"`
	// Lost: the guest rebooted, the VM stopped or the Guest Service was
	// replaced before the job exited.
	Lost bool `json:"lost"`
	// Dropped: output older than glovd's 16 MiB window was skipped.
	Dropped bool `json:"dropped"`
}

// ConsoleParams are vm.console's.
type ConsoleParams struct {
	VM string `json:"vm"`
	// Log and Offset are the previous read's, empty and 0 at first.
	Log      string `json:"log,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
	MaxBytes int    `json:"maxBytes,omitempty"`
}

// MaxConsoleRead bounds one vm.console read.
const MaxConsoleRead = 256 << 10

// ConsoleRead is vm.console's result: the VM's serial console output from
// Offset on, as the guest wrote it, control characters too.
type ConsoleRead struct {
	Data []byte `json:"data"`
	// Log names this boot's log; Offset is where the next read starts.
	Log    string `json:"log"`
	Offset int64  `json:"offset"`
	// Restarted: the VM booted again since the previous read, whose log
	// is gone; Data starts this boot's.
	Restarted bool `json:"restarted"`
	Running   bool `json:"running"`
}

// ExecKillParams are exec.kill's.
type ExecKillParams struct {
	VM    string `json:"vm"`
	JobID string `json:"jobId"`
}

// GuestCallParams are guest.call's: one of the Guest Service's file
// methods (docs/guest-protocol.md) with its params; the result is the
// method's.
type GuestCallParams struct {
	VM     string         `json:"vm"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params,omitempty"`
}

// StopParams are vm.stop's.
type StopParams struct {
	VM string `json:"vm"`
	// Force turns the VM off without asking the guest to shut down.
	Force bool `json:"force,omitempty"`
}

// RebootParams are vm.reboot's.
type RebootParams struct {
	VM string `json:"vm"`
	// Timeout is in seconds; zero means DefaultRebootTimeout.
	Timeout float64 `json:"timeout,omitempty"`
}

// WaitParams are vm.wait's.
type WaitParams struct {
	VM string `json:"vm"`
	// State is "running" (the default) or "guest-ready".
	State string `json:"state,omitempty"`
	// Timeout is in seconds; zero means DefaultWaitTimeout.
	Timeout float64 `json:"timeout,omitempty"`
}

// Wait states and timeouts, as on macOS.
const (
	WaitRunning          = "running"
	WaitGuestReady       = "guest-ready"
	DefaultWaitTimeout   = 60
	DefaultRebootTimeout = 600
)

// VMInfo is a VM's state, as vm.list and vm.status return it.
type VMInfo struct {
	Name string `json:"name"`
	// "running" or "stopped".
	State string `json:"state"`
	// Always "hcs" on Windows ("qemu" or "vz" on macOS).
	Engine string `json:"engine"`
	// Isolated VMs reach out only through the egress proxy.
	Isolated bool `json:"isolated,omitzero"`
	// Harnesses are the profiles it was created with, which glov vm harness
	// install installs.
	Harnesses []string `json:"harnesses,omitempty"`
	// Template: the VM's disk is frozen, and glov vm reset starts it from
	// there again.
	Template bool `json:"template,omitzero"`
	// Disk counts the disks glov vm reset gave the VM (store.Record.Disk).
	Disk int `json:"disk,omitzero"`
	// Project: the VM is a project's (glov claude).
	Project *Project `json:"project,omitempty"`
	// Only while the VM runs.
	GuestService *GuestService `json:"guestService,omitempty"`
}

// GuestService is the host's view of the VM's Guest Service connection.
type GuestService struct {
	// "ready", "connecting" or "unresponsive".
	State   string  `json:"state"`
	OS      *string `json:"os,omitempty"`
	Version *string `json:"version,omitempty"`
	User    *string `json:"user,omitempty"`
	BootID  *string `json:"bootId,omitempty"`
	Problem *string `json:"problem,omitempty"`
	// ConnectionID changes each time a Guest Service session is
	// established, so an update can tell the new one from the old.
	ConnectionID *string `json:"connectionID,omitempty"`
	// Update is vm.status's check against glovd's Guest Service, made
	// once per connection; vm.list leaves it out.
	Update *GuestCheck `json:"update,omitempty"`
}

// Guest Service update states, as on macOS.
const (
	UpdateUpToDate  = "up-to-date"
	UpdateAvailable = "update-available"
	UpdateUnknown   = "unknown"
)

// GuestCheck is guest.check's result and vm.status's guestService.update.
type GuestCheck struct {
	State          string  `json:"state"`
	CurrentVersion *string `json:"currentVersion,omitempty"`
	TargetVersion  *string `json:"targetVersion,omitempty"`
	Problem        *string `json:"problem,omitempty"`
}

// Describe is the line glov vm guest check prints, as on macOS.
func (c GuestCheck) Describe() string {
	return printable(c.describe())
}

func (c GuestCheck) describe() string {
	or := func(s *string) string {
		if s == nil {
			return "unknown"
		}
		return *s
	}
	s := "Guest Service " + or(c.CurrentVersion) + " -> " + or(c.TargetVersion) + ": " + c.State
	if c.Problem != nil {
		s += " (" + *c.Problem + ")"
	}
	return s
}

// GuestUpdateParams are guest.update's.
type GuestUpdateParams struct {
	VM string `json:"vm"`
	// Force cuts off running exec jobs and share syncs.
	Force bool `json:"force,omitempty"`
	// Timeout is in seconds; zero means DefaultGuestUpdateTimeout.
	Timeout float64 `json:"timeout,omitempty"`
}

// DefaultGuestUpdateTimeout bounds the wait for the new Guest Service, and
// MaxGuestUpdateTimeout a timeout given, as on macOS.
const (
	DefaultGuestUpdateTimeout = 120
	MaxGuestUpdateTimeout     = 3600
)

// VMDeleted is vm.delete's result: the base images no VM used any more.
type VMDeleted struct {
	PrunedImages []PrunedImage `json:"prunedImages,omitzero"`
}

// PrunedImage is a base image removed from glovd's cache because no
// VM used it and a newer build of its distro is cached.
type PrunedImage struct {
	Release string `json:"release"`
	Bytes   int64  `json:"bytes"`
}

// Describe is the line glov prints for im.
func (im PrunedImage) Describe() string {
	size := fmt.Sprintf("%.1f GiB", float64(im.Bytes)/(1<<30))
	if im.Bytes < 1<<30 {
		size = fmt.Sprintf("%d MiB", im.Bytes>>20)
	}
	return printable("removed unused base image " + im.Release + " (" + size + ")")
}

// GuestUpdated is guest.update's result; Updated is false when the
// installed Guest Service already matched.
type GuestUpdated struct {
	Updated bool   `json:"updated"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// printable drops control characters: versions and problems can quote the
// guest's own text, which must not drive the terminal glov prints them to.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Describe is the one-line text glov prints for a VM, as on macOS.
func (v VMInfo) Describe() string {
	return printable(v.describe())
}

func (v VMInfo) describe() string {
	s := v.Name + ": " + v.State
	if v.Engine != "" {
		s += " [" + v.Engine + "]"
	}
	if v.Template {
		s += ", frozen (glov vm reset returns it to its template)"
	}
	if g := v.GuestService; g != nil {
		s += ", Guest Service " + g.State
		if g.Problem != nil {
			s += " (" + *g.Problem + ")"
		}
		if g.Update != nil && g.Update.State == UpdateAvailable {
			s += ", update available: glov vm guest update " + v.Name
		}
	}
	return s
}

// ShareAddParams are share.add's. Share.HostPath must be canonical
// (share.Canonical); glov checks the folder itself.
type ShareAddParams struct {
	VM    string      `json:"vm"`
	Share share.Share `json:"share"`
}

// ShareParams name one share of a VM.
type ShareParams struct {
	VM   string `json:"vm"`
	Name string `json:"name"`
}

// ShareInfo is a share as share.add and share.list return it.
type ShareInfo struct {
	share.Share `json:",inline"`
	GuestPath   string `json:"guestPath"`
}

// NewShareInfo adds s's guest path.
func NewShareInfo(s share.Share) ShareInfo { return ShareInfo{Share: s, GuestPath: s.GuestPath()} }

// Share sync. glov scans the host folder and reads its files as the user;
// glovd holds the share for one sync at a time, passes sync.manifest and
// sync.apply on to the Guest Service with the share's id and guest root,
// watches a long guest listing, and cancels the guest's work when glov goes
// away (docs/guest-protocol.md, "Share sync").
const (
	// MethodShareSyncBegin takes the share for this connection's sync.
	MethodShareSyncBegin = "share.syncBegin"
	// MethodShareSyncCall passes sync.manifest or sync.apply on.
	MethodShareSyncCall = "share.syncCall"
	// MethodShareSyncProgress records the host's stage for share.status.
	MethodShareSyncProgress = "share.syncProgress"
	// MethodShareSyncEnd frees the share; Cancel stops the guest's work.
	MethodShareSyncEnd = "share.syncEnd"
	// MethodShareStatus lists the VM's running syncs.
	MethodShareStatus = "share.status"
)

// Live shares (share.Live): glov share agent, running as the user, keeps the
// host folder and the guest copy in step. It holds the share for as long as
// its connection lasts; share.syncCall also passes sync.watch, sync.read
// and sync.unwatch on for it, and glovd sends it these notifications.
const (
	// MethodShareLiveBegin takes a live share for this connection.
	MethodShareLiveBegin = "share.liveBegin"
	// MethodShareLiveConflict logs a path both sides changed.
	MethodShareLiveConflict = "share.liveConflict"
	// MethodShareLiveLocks reports the git lock files that have held a
	// live share's git data long (ShareLiveLocksParams).
	MethodShareLiveLocks = "share.liveLocks"
	// MethodShareLiveReconcile asks the share's agent for a full round.
	MethodShareLiveReconcile = "share.liveReconcile"

	// NotifyShareChanged carries the guest's sync.changed (ShareChanged).
	NotifyShareChanged = "share.changed"
	// NotifyShareRewatch: the Guest Service (re)connected, so its watch is
	// gone: watch again and run a full round (ShareParams).
	NotifyShareRewatch = "share.rewatch"
	// NotifyShareReconcile: glov share sync asked for a full round
	// (ShareParams).
	NotifyShareReconcile = "share.reconcile"
)

// ShareChanged is share.changed's params.
type ShareChanged struct {
	VM       string   `json:"vm"`
	Name     string   `json:"name"`
	Paths    []string `json:"paths,omitempty"`
	Overflow bool     `json:"overflow,omitzero"`
	Error    string   `json:"error,omitempty"`
}

// ShareLiveLocksParams are share.liveLocks's: Locks are "<side> <path>",
// none once they are gone.
type ShareLiveLocksParams struct {
	VM    string   `json:"vm"`
	Name  string   `json:"name"`
	Locks []string `json:"locks"`
}

// ShareLiveConflictParams are share.liveConflict's: Winner is "host" or
// "guest", the side whose version was kept.
type ShareLiveConflictParams struct {
	VM     string `json:"vm"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Winner string `json:"winner"`
	Why    string `json:"why"`
	// Kept is the branch the other side's version of a branch ref was
	// kept as.
	Kept string `json:"kept,omitempty"`
}

// ShareSyncBeginParams are share.syncBegin's; Label is <name>[/<subdir>].
type ShareSyncBeginParams struct {
	VM    string `json:"vm"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

// ShareSyncBegun is share.syncBegin's result: the VM's full name and the
// share as vm.json holds it.
type ShareSyncBegun struct {
	VM    string    `json:"vm"`
	Share ShareInfo `json:"share"`
	// Disk is the VM's disk (store.Record.Disk) for a live share's agent.
	Disk int `json:"disk,omitzero"`
}

// ShareSyncCallParams are share.syncCall's: Params are the Guest Service
// method's, without id and root, which glovd sets.
type ShareSyncCallParams struct {
	VM     string         `json:"vm"`
	Name   string         `json:"name"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
}

// Sync stages, as on macOS.
const (
	StageHostScan     = "host scan"
	StageGuestListing = "guest listing"
	StageApplying     = "applying"
	// StageLive is a live share's agent, between rounds or in one.
	StageLive = "live"
)

// ShareSyncProgressParams are share.syncProgress's.
type ShareSyncProgressParams struct {
	VM         string `json:"vm"`
	Name       string `json:"name"`
	Stage      string `json:"stage"`
	SentBytes  int64  `json:"sentBytes"`
	TotalBytes int64  `json:"totalBytes"`
	// InStep: a live share's first full round is done.
	InStep bool `json:"inStep,omitempty"`
}

// ShareSyncEndParams are share.syncEnd's.
type ShareSyncEndParams struct {
	VM     string `json:"vm"`
	Name   string `json:"name"`
	Cancel bool   `json:"cancel,omitempty"`
}

// ShareStatusParams are share.status's; an empty Name means every share.
type ShareStatusParams struct {
	VM   string `json:"vm"`
	Name string `json:"name,omitempty"`
}

// SyncProgress is a running sync, as macOS reports it.
type SyncProgress struct {
	Share      string  `json:"share"`
	Stage      string  `json:"stage"`
	Seconds    float64 `json:"seconds"`
	SentBytes  int64   `json:"sentBytes"`
	TotalBytes int64   `json:"totalBytes"`
	// Guest is the Guest Service's sync.status while it works on the
	// share, nil otherwise.
	Guest *GuestSyncProgress `json:"guest,omitempty"`
	// Conflicts are a live share's latest, newest last.
	Conflicts []ShareLiveConflictParams `json:"conflicts,omitempty"`
	// Locks are a live share's git lock files held long (share.liveLocks).
	Locks []string `json:"locks,omitempty"`
	// InStep: a live share's agent finished its first full round.
	InStep bool `json:"inStep,omitempty"`
}

// GuestSyncProgress is sync.status's result while busy.
type GuestSyncProgress struct {
	Phase   string  `json:"phase"`
	Path    string  `json:"path"`
	Files   int64   `json:"files"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
}

// StatsParams are vm.stats'; an empty VM means every running VM.
type StatsParams struct {
	VM string `json:"vm,omitempty"`
}

// Stats is vm.stats' result: the host's memory and one row per running VM.
type Stats struct {
	Host HostMemory `json:"host"`
	VMs  []VMStats  `json:"vms"`
}

// HostMemory is the host's memory in bytes (GlobalMemoryStatusEx): physical
// memory, and the commit charge against its limit (RAM plus page files).
type HostMemory struct {
	Physical    uint64 `json:"physical"`
	Available   uint64 `json:"available"`
	CommitUsed  uint64 `json:"commitUsed"`
	CommitLimit uint64 `json:"commitLimit"`
}

// VMStats is one running VM from the host's side and, when its Guest
// Service answers, from the guest's. Sizes are bytes.
type VMStats struct {
	Name string `json:"name"`
	CPUs int    `json:"cpus"`
	// Memory is the configured size.
	Memory uint64 `json:"memory"`
	// CPUPercent is the VM's processor time over the sample: 100 is one
	// host core.
	CPUPercent *float64 `json:"cpuPercent,omitempty"`
	// MemoryResident is the guest memory in host RAM and MemoryCommitted
	// what the host has committed for it: all of its configured size.
	MemoryResident  *uint64 `json:"memoryResident,omitempty"`
	MemoryCommitted *uint64 `json:"memoryCommitted,omitempty"`
	// DiskAllocated is the VM's own disk file on the host (its base image
	// is shared); DiskSize is the size the guest sees.
	DiskAllocated *uint64 `json:"diskAllocated,omitempty"`
	DiskSize      uint64  `json:"diskSize"`
	// HostProblem says why some host-side numbers are missing.
	HostProblem *string     `json:"hostProblem,omitempty"`
	Guest       *GuestStats `json:"guest,omitempty"`
	// GuestProblem says why Guest is missing.
	GuestProblem *string `json:"guestProblem,omitempty"`
}

// GuestStats is guest.stats' result (docs/guest-protocol.md).
type GuestStats struct {
	MemoryTotal     uint64    `json:"memoryTotal"`
	MemoryAvailable uint64    `json:"memoryAvailable"`
	DiskPath        string    `json:"diskPath"`
	DiskTotal       uint64    `json:"diskTotal"`
	DiskUsed        uint64    `json:"diskUsed"`
	DiskAvailable   uint64    `json:"diskAvailable"`
	Load            []float64 `json:"load"`
	CPUBusy         float64   `json:"cpuBusy"`
}

// EgressParams are egress.list's (no hosts), egress.allow's and
// egress.remove's.
type EgressParams struct {
	VM    string   `json:"vm"`
	Hosts []string `json:"hosts,omitempty"`
	// MCP is set by glov mcp on every change it relays: refused unless
	// the VM opted in (egress.mcp).
	MCP bool `json:"mcp,omitzero"`
	// Private (egress.allow): the hosts may also resolve to private-network
	// addresses (--private); refused with MCP.
	Private bool `json:"private,omitzero"`
}

// EgressModeParams are egress.mode's: open (any public host, through the
// proxy) or restricted (the allowlist). Opening a VM that holds tmpfs
// secrets needs AllowPlainSecrets.
type EgressModeParams struct {
	VM                string `json:"vm"`
	Open              bool   `json:"open"`
	AllowPlainSecrets bool   `json:"allowPlainSecrets,omitzero"`
}

// EgressMCPParams are egress.mcp's: whether MCP clients may change the
// VM's allowlist. glov mcp relays no tool for it.
type EgressMCPParams struct {
	VM      string `json:"vm"`
	Enabled bool   `json:"enabled"`
}

// Egress is an isolated VM's egress policy.
type Egress struct {
	VM       string   `json:"vm"`
	Allow    []string `json:"allow"`
	MCPEdits bool     `json:"mcpEdits"`
	// Private are the Allow entries that may resolve to private-network
	// addresses.
	Private []string `json:"private,omitempty"`
	// Open: the VM may reach any public host (egress.mode); Allow then
	// waits for a switch back.
	Open bool `json:"open,omitzero"`
}

// TrustParams are trust.list's (VM), trust.add's (VM, Name and one PEM
// Certificate, replacing one of that name) and trust.remove's (VM, Name).
type TrustParams struct {
	VM          string `json:"vm"`
	Name        string `json:"name,omitempty"`
	Certificate string `json:"certificate,omitempty"`
}

// Trust is the CA certificates an isolated VM trusts.
type Trust struct {
	VM           string               `json:"vm"`
	Certificates []TrustedCertificate `json:"certificates"`
}

// TrustedCertificate describes one: SHA256 is of the DER certificate, in
// lowercase hex; Authority is whether it says it is a CA (a self-signed
// server certificate works as trust too).
type TrustedCertificate struct {
	Name      string     `json:"name"`
	Subject   string     `json:"subject"`
	NotAfter  *time.Time `json:"notAfter,omitempty"`
	SHA256    string     `json:"sha256"`
	Authority bool       `json:"authority"`
}

// SecretParams are secret.set's (Name, Hosts, Value), secret.source's (the
// same and Command, whose stdout glov just read as Value),
// secret.refresh's (Name and Value) and secret.remove's (Name).
type SecretParams struct {
	Name    string   `json:"name,omitempty"`
	Hosts   []string `json:"hosts,omitempty"`
	Value   string   `json:"value,omitempty"`
	Command []string `json:"command,omitempty"`
	// Forge, github or gitlab, makes secret.set's and secret.source's
	// secret a git sign-in, whose hosts are held to each VM's repositories
	// (secret.repos).
	Forge string `json:"forge,omitempty"`
}

// Secret is a secret as secret.list shows it: never its value. Length and
// Ending are a stored value's.
type Secret struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Command []string `json:"command,omitempty"`
	Hosts   []string `json:"hosts"`
	Length  int      `json:"length,omitzero"`
	Ending  string   `json:"ending,omitempty"`
	Forge   string   `json:"forge,omitempty"`
}

// Secrets is secret.list's result.
type Secrets struct {
	Secrets []Secret `json:"secrets"`
}

// SecretGrantParams are secret.grant's and secret.revoke's (VM and Names)
// and secret.grants' (VM).
type SecretGrantParams struct {
	VM    string   `json:"vm"`
	Names []string `json:"names,omitempty"`
	// AllowPlainSecrets lets an open VM hold the tmpfs secrets among Names.
	AllowPlainSecrets bool `json:"allowPlainSecrets,omitzero"`
}

// SecretGrants is the secrets an isolated VM may use; Missing names those
// no longer defined.
type SecretGrants struct {
	VM      string   `json:"vm"`
	Names   []string `json:"names"`
	Missing []string `json:"missing"`
}

// SecretReposParams are secret.repos': Add and Remove change the
// repositories (<host>/<owner>/<repo>, <host>/<group>/*) isolated VM VM may
// reach with its git secrets; with neither it only answers.
type SecretReposParams struct {
	VM     string   `json:"vm"`
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// SecretRepos is secret.repos' result: the VM's repositories.
type SecretRepos struct {
	VM    string   `json:"vm"`
	Repos []string `json:"repos"`
}

// SecretLoginParams are secret.login's and secret.logout's: Value for a
// harness whose token is pasted.
type SecretLoginParams struct {
	VM      string `json:"vm"`
	Harness string `json:"harness"`
	Value   string `json:"value,omitempty"`
}

// SecretLogin is secret.login's and secret.logout's result.
type SecretLogin struct {
	VM      string `json:"vm"`
	Harness string `json:"harness"`
	// Command, after secret.login, is what the owner runs in the VM, as
	// the guest user in a terminal, to sign in; empty when the token was
	// pasted.
	Command []string `json:"command,omitempty"`
	// Missing are the harness's subscription secrets not stored yet: a
	// sign-in in the guest stores them.
	Missing []string `json:"missing"`
}

// EgressLogParams are egress.log's: with Cursor (from an earlier answer)
// only what came after it, else the last Limit records.
type EgressLogParams struct {
	VM string `json:"vm"`
	// Denied keeps only deny and fail records.
	Denied bool   `json:"denied,omitzero"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitzero"`
}

// DefaultEgressLogLimit and MaxEgressLogLimit bound egress.log without a
// cursor.
const (
	DefaultEgressLogLimit = 100
	MaxEgressLogLimit     = 10000
)
