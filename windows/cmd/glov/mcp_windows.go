package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/jobrun"
)

// glov mcp serves the macOS edition's MCP tools (Sources/glov/MCPCommand.swift)
// over stdio, with the same names, descriptions and input schemas where this
// edition has the feature. Tools it lacks (screenshot and input, snapshots)
// are absent, not failing at call time. Each call dials glovd as
// the user, so glovd authorizes it against the owner's SID as it does
// the CLI, and host files are read and written as the user.

// conn is one glovd connection.
type conn interface {
	caller
	Close() error
}

type mcpServer struct {
	dial func(ctx context.Context) (conn, error)
}

func runMCP(ctx context.Context) error {
	s := &mcpServer{dial: func(ctx context.Context) (conn, error) {
		return dialGlovd(ctx, printProgress)
	}}
	return s.server().Run(ctx, &mcp.StdioTransport{})
}

func (s *mcpServer) server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "glov", Version: version}, nil)
	for _, t := range mcpTools {
		srv.AddTool(t.tool(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, err := s.call(ctx, t, req.Params.Arguments)
			if err != nil {
				r := &mcp.CallToolResult{}
				r.Content = []mcp.Content{&mcp.TextContent{Text: err.Error()}}
				r.IsError = true
				return r, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	}
	return srv
}

func (s *mcpServer) call(ctx context.Context, t mcpTool, raw []byte) (string, error) {
	var a mcpArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("arguments: %v", err)
		}
	}
	for _, r := range t.required {
		if !a.has(r) {
			return "", fmt.Errorf("%s needs %s", t.name, r)
		}
	}
	c, err := s.dial(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return t.run(ctx, c, a)
}

// mcpArgs holds every tool's arguments; each tool reads its own.
type mcpArgs struct {
	VM        string            `json:"vm"`
	Action    string            `json:"action"`
	CPUs      int               `json:"cpus"`
	Memory    string            `json:"memory"`
	DiskSize  string            `json:"diskSize"`
	Force     bool              `json:"force"`
	Timeout   float64           `json:"timeout"`
	State     string            `json:"state"`
	Argv      []string          `json:"argv"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	As        string            `json:"as"`
	JobID     string            `json:"jobId"`
	Cursor    mcpCursor         `json:"cursor"`
	MaxBytes  int               `json:"maxBytes"`
	Path      string            `json:"path"`
	Text      *string           `json:"text"`
	HostPath  string            `json:"hostPath"`
	GuestPath string            `json:"guestPath"`
	Share     string            `json:"share"`
	Verify    bool              `json:"verify"`
	Hosts     []string          `json:"hosts"`
	Denied    bool              `json:"denied"`
	Limit     int               `json:"limit"`
}

// mcpCursor is exec_read's cursor, a number, or egress_log's, a string.
type mcpCursor struct {
	n int
	s string
}

func (c *mcpCursor) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.s)
	}
	return json.Unmarshal(b, &c.n)
}

func (a mcpArgs) has(name string) bool {
	switch name {
	case "vm":
		return a.VM != ""
	case "action":
		return a.Action != ""
	case "argv":
		return len(a.Argv) > 0
	case "jobId":
		return a.JobID != ""
	case "path":
		return a.Path != ""
	case "text":
		return a.Text != nil
	case "hostPath":
		return a.HostPath != ""
	case "guestPath":
		return a.GuestPath != ""
	case "share":
		return a.Share != ""
	case "hosts":
		return len(a.Hosts) > 0
	}
	return true
}

type mcpTool struct {
	name, description string
	properties        map[string]any
	required          []string
	run               func(ctx context.Context, c conn, a mcpArgs) (string, error)
}

func (t mcpTool) tool() *mcp.Tool {
	props := maps.Clone(t.properties)
	if props == nil {
		props = map[string]any{}
	}
	if slices.Contains(t.required, "vm") {
		props["vm"] = prop("string", "VM name or unique name prefix (see vm_list).")
	}
	required := t.required
	if required == nil {
		required = []string{}
	}
	return &mcp.Tool{Name: t.name, Description: t.description,
		InputSchema: map[string]any{"type": "object", "properties": props, "required": required}}
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

const shellHint = `Runs argv directly with no shell: pass one explicitly, e.g. ["/bin/sh", "-c", "..."].`

const allowlistHint = "The host path must be inside one of the VM's shares (a rw one when copying from the guest; glov share add) or %LOCALAPPDATA%\\glov\\scratch; anything else is refused."

var (
	jobIDProp      = prop("string", "Job id returned by exec_start or exec.")
	execProperties = map[string]any{
		"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `Program and arguments, e.g. ["make", "-j4"].`},
		"cwd":  prop("string", "Working directory in the guest."),
		"env":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Extra environment variables."},
		"as":   prop("string", `"system" (default: root) or "user" (the signed-in console user).`),
	}
)

func with(base map[string]any, extra map[string]any) map[string]any {
	m := maps.Clone(base)
	maps.Copy(m, extra)
	return m
}

// jsonText is the default reply: the result as JSON, as on macOS.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func vmCall(method string, params func(a mcpArgs) (any, error)) func(context.Context, conn, mcpArgs) (string, error) {
	return func(ctx context.Context, c conn, a mcpArgs) (string, error) {
		p, err := params(a)
		if err != nil {
			return "", err
		}
		var vm api.VMInfo
		if err := call(ctx, c, method, p, &vm); err != nil {
			return "", err
		}
		return jsonText(vm)
	}
}

func vmOnly(a mcpArgs) (any, error) { return api.VMParams{VM: a.VM}, nil }

// sizes turns the macOS tools' cpus, memory and diskSize into glovd's.
func sizes(a mcpArgs) (cpus, memoryMiB, diskGiB int, err error) {
	if a.CPUs < 0 {
		return 0, 0, 0, errors.New("cpus: want a positive number")
	}
	if a.Memory != "" {
		if memoryMiB, err = sizeMiB(a.Memory, 1); err != nil {
			return 0, 0, 0, fmt.Errorf("memory %s: %v", a.Memory, err)
		}
	}
	if a.DiskSize != "" {
		if diskGiB, err = parseDiskGiB(a.DiskSize); err != nil {
			return 0, 0, 0, fmt.Errorf("diskSize %s: %v", a.DiskSize, err)
		}
	}
	return a.CPUs, memoryMiB, diskGiB, nil
}

var mcpTools = []mcpTool{
	{name: "vm_list", description: "List the configured VMs, whether each is running and its engine (hcs: Linux on Hyper-V).",
		run: func(ctx context.Context, c conn, _ mcpArgs) (string, error) {
			var vms []api.VMInfo
			if err := call(ctx, c, api.MethodVMList, nil, &vms); err != nil {
				return "", err
			}
			return jsonText(vms)
		}},
	{name: "vm_status", required: []string{"vm"}, description: "Show VM state and its Guest Service's state.",
		run: vmCall(api.MethodVMStatus, vmOnly)},
	{name: "vm_configure", required: []string{"vm"},
		description: "Reconfigure a stopped VM's CPU count, memory or virtual disk size (disk size only grows; never shrinks). Stop the VM with vm_stop first.",
		properties: map[string]any{
			"cpus":     prop("integer", "vCPUs (at least 1)."),
			"memory":   prop("string", "Memory size, e.g. 8G."),
			"diskSize": prop("string", "Virtual disk size, e.g. 120G (grows the disk, never shrinks)."),
		},
		run: vmCall(api.MethodVMConfigure, func(a mcpArgs) (any, error) {
			cpus, mem, disk, err := sizes(a)
			if err == nil && cpus == 0 && mem == 0 && disk == 0 {
				err = errors.New("pass at least one of cpus, memory or diskSize")
			}
			return api.ConfigureParams{VM: a.VM, CPUs: cpus, MemoryMiB: mem, DiskGiB: disk}, err
		})},
	{name: "vm_start", required: []string{"vm"}, description: "Start a VM. It keeps running when this MCP session ends. Follow with vm_wait guest-ready.",
		run: vmCall(api.MethodVMStart, vmOnly)},
	{name: "vm_stop", required: []string{"vm"}, description: "Shut a VM down: a graceful power-down (waits up to 60 s, then forces) or, with force, immediately.",
		properties: map[string]any{"force": prop("boolean", "Skip the graceful shutdown.")},
		run:        vmCall(api.MethodVMStop, func(a mcpArgs) (any, error) { return api.StopParams{VM: a.VM, Force: a.Force}, nil })},
	{name: "vm_reboot", required: []string{"vm"},
		description: "Restart a VM, e.g. after installing software that needs a restart, and wait until the new boot's Guest Service is ready: the result is vm_status's with the new bootId. Running exec jobs end. MCP clients time out around 60 s: note bootId from vm_status first, pass a timeout under 50, and after a timeout poll vm_status until it is ready with another bootId rather than calling vm_reboot again.",
		properties:  map[string]any{"timeout": prop("number", fmt.Sprintf("Seconds to wait for the new boot (default %d).", api.DefaultRebootTimeout))},
		run:         vmCall(api.MethodVMReboot, func(a mcpArgs) (any, error) { return api.RebootParams{VM: a.VM, Timeout: a.Timeout}, nil })},
	{name: "vm_wait", required: []string{"vm"},
		description: "Wait until a VM is running, or (guest-ready) until its Guest Service answers. Stay under about 50 s per call and call again: MCP clients time out around 60 s.",
		properties: map[string]any{
			"state":   prop("string", `"running" (default) or "guest-ready".`),
			"timeout": prop("number", "Seconds to wait (default 60)."),
		},
		run: vmCall(api.MethodVMWait, func(a mcpArgs) (any, error) {
			return api.WaitParams{VM: a.VM, State: a.State, Timeout: a.Timeout}, nil
		})},
	{name: "exec", required: []string{"vm", "argv"},
		description: "Run a command in the guest and wait. " + shellHint + " Returns after at most `timeout` seconds (default 30, max 50) either way: with the exit code, or, when the command is still running, with a job id to follow using exec_read (use exec_start for work you know is long, such as builds). The reply holds the exit code, the last 50 lines of output (stdout and stderr interleaved) and the line count, never the whole output; exec_read from cursor 0 reads all of it.",
		properties:  with(execProperties, map[string]any{"timeout": prop("number", "Seconds to wait, default 30, at most 50. The command keeps running past it.")}),
		run:         execRun},
	{name: "exec_start", required: []string{"vm", "argv"},
		description: "Start a command in the guest and return a job id at once, for long work such as builds or installs. Follow it with exec_read, stop it with exec_kill. " + shellHint,
		properties:  execProperties,
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			id, err := execStart(ctx, c, a)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("job %s started; read it with exec_read (jobId %q, cursor 0)", id, id), nil
		}},
	{name: "exec_read", required: []string{"vm", "jobId"},
		description: "Read a job's output from a cursor (a byte offset into its total output; start at 0 and pass back the returned cursor). Returns the new text, the new cursor and whether the job exited. glovd keeps the last 16 MiB of a job's output; if the cursor fell out of it the reply says so.",
		properties: map[string]any{
			"jobId":    jobIDProp,
			"cursor":   prop("integer", "Byte offset to continue from (default 0)."),
			"maxBytes": prop("integer", "At most this many bytes (default 32768, max 262144)."),
		},
		run: execRead},
	{name: "exec_kill", required: []string{"vm", "jobId"}, description: "Kill a running job's process.",
		properties: map[string]any{"jobId": jobIDProp},
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			if err := call(ctx, c, api.MethodExecKill, api.ExecKillParams{VM: a.VM, JobID: a.JobID}, nil); err != nil {
				return "", err
			}
			return "job " + a.JobID + " killed", nil
		}},
	{name: "file_read", required: []string{"vm", "path"},
		description: "Read a UTF-8 text file in the guest. Files over 256 KiB are refused: use copy_from_vm for those and for binary files.",
		properties: map[string]any{
			"path":     prop("string", "Path in the guest, e.g. /var/log/app.log."),
			"maxBytes": prop("integer", "Refuse files larger than this (at most 262144)."),
		},
		run: fileRead},
	{name: "file_write", required: []string{"vm", "path", "text"},
		description: "Write a UTF-8 text file in the guest, replacing it and creating missing directories.",
		properties:  map[string]any{"path": prop("string", "Path in the guest."), "text": prop("string", "File content.")},
		run:         fileWrite},
	{name: "copy_to_vm", required: []string{"vm", "hostPath", "guestPath"},
		description: "Copy a file or a directory tree from the host into the guest. A directory's contents land at guestPath (missing directories are created; symbolic links are skipped). " + allowlistHint,
		properties:  map[string]any{"hostPath": prop("string", "Absolute path on the host."), "guestPath": prop("string", "Destination path in the guest.")},
		run:         copyTo},
	{name: "copy_from_vm", required: []string{"vm", "guestPath", "hostPath"},
		description: "Copy a file or a directory tree from the guest to the host. A directory's contents land at hostPath. " + allowlistHint,
		properties:  map[string]any{"guestPath": prop("string", "Source path in the guest."), "hostPath": prop("string", "Absolute destination path on the host.")},
		run:         copyFrom},
	{name: "share_sync", required: []string{"vm", "share"},
		description: "Mirror a share's host folder (glov share add) into its guest folder (/mnt/<drive>/<path>) on a running VM, before a build or a docker run for example: new and changed files are written (only when their SHA-256 differs), files deleted on the host are deleted in the guest, excluded paths are left alone. Files changed in the guest are overwritten and listed. A first sync of a large tree can take minutes; later ones move only what changed. A live share (glov share add --live) is kept in step both ways already: this only asks its agent for a full comparison.",
		properties: map[string]any{
			"share":  prop("string", "Share name, or name/subdir to sync one directory of it."),
			"verify": prop("boolean", "Rehash every file on both sides instead of trusting unchanged stat keys."),
		},
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			return syncShare(ctx, c, a.VM, a.Share, a.Verify)
		}},
	{name: "egress_list", required: []string{"vm"},
		description: "Show an isolated VM's egress allowlist: the only hosts it can reach, through the egress proxy; or, when the user opened it (glov vm configure --egress open), that it may reach any public host.",
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			return egressCall(ctx, c, api.MethodEgressList, api.EgressParams{VM: a.VM})
		}},
	{name: "egress_allow", required: []string{"vm", "hosts"},
		description: "Add hosts to an isolated VM's egress allowlist; applies to its next connection, no restart. An entry is a hostname, *.suffix (names below suffix) or an IP address, for ports 443 and 80, or host:port for that port only. Private and link-local addresses and the host's own stay refused; only the user can allow a private-network host (glov egress allow --private). Keep the list minimal: every allowed host is a channel out. Refused unless the user opted the VM in (glov egress mcp <vm> on): ask them first.",
		properties:  map[string]any{"hosts": egressHosts},
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			// glovd refuses an MCP client's policy change unless the VM
			// opted in; the client cannot unset this.
			return egressCall(ctx, c, api.MethodEgressAllow, api.EgressParams{VM: a.VM, Hosts: a.Hosts, MCP: true})
		}},
	{name: "egress_remove", required: []string{"vm", "hosts"},
		description: "Remove hosts from an isolated VM's egress allowlist; applies to its next connection. Refused unless the user opted the VM in (glov egress mcp <vm> on).",
		properties:  map[string]any{"hosts": egressHosts},
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			return egressCall(ctx, c, api.MethodEgressRemove, api.EgressParams{VM: a.VM, Hosts: a.Hosts, MCP: true})
		}},
	{name: "egress_log", required: []string{"vm"},
		description: "An isolated VM's egress connections, newest last: UTC time, allow/deny/fail, host:port, bytes in and out, reason. Pass the returned cursor back to get only newer ones.",
		properties: map[string]any{
			"denied": prop("boolean", "Only connections that did not go through (deny and fail)."),
			"cursor": prop("string", "From a previous egress_log answer: return only what came after it."),
			"limit":  prop("integer", fmt.Sprintf("Without a cursor: how many of the latest connections (default %d).", api.DefaultEgressLogLimit)),
		},
		run: func(ctx context.Context, c conn, a mcpArgs) (string, error) {
			var page egress.Page
			if err := call(ctx, c, api.MethodEgressLog, api.EgressLogParams{VM: a.VM, Denied: a.Denied, Cursor: a.Cursor.s, Limit: a.Limit}, &page); err != nil {
				return "", err
			}
			lines := []string{}
			for _, r := range page.Records {
				lines = append(lines, describeEgress(r))
			}
			if len(lines) == 0 {
				lines = append(lines, "no connections")
			}
			return strings.Join(append(lines, "cursor: "+page.Cursor), "\n"), nil
		}},
}

var egressHosts = map[string]any{"type": "array", "items": map[string]any{"type": "string"},
	"description": `Entries, e.g. ["api.example.com", "*.ubuntu.com", "registry.example.org:8443"].`}

// egressCall sends an egress allowlist method and describes the result.
func egressCall(ctx context.Context, c conn, method string, p api.EgressParams) (string, error) {
	var e api.Egress
	if err := call(ctx, c, method, p, &e); err != nil {
		return "", err
	}
	return describeAllowlist(e), nil
}

func execStart(ctx context.Context, c conn, a mcpArgs) (string, error) {
	var job api.JobStarted
	err := call(ctx, c, api.MethodExecStart, api.ExecParams{VM: a.VM, Argv: a.Argv, Cwd: a.Cwd, Env: a.Env, As: a.As}, &job)
	return job.JobID, err
}

// tailLines is how much of its output exec returns.
const tailLines = 50

// execRun starts a command and follows it for up to timeout seconds,
// keeping its line count and the end of its output.
func execRun(ctx context.Context, c conn, a mcpArgs) (string, error) {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	timeout = min(timeout, 50)
	id, err := execStart(ctx, c, a)
	if err != nil {
		return "", err
	}
	t := &tailWriter{last: '\n'}
	res, err := jobrun.Run(ctx, rpcJob{c, a.VM, id}, t, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		return "", err
	}
	lines := t.lines
	if t.last != '\n' {
		lines++
	}
	return describeRun(id, res, lastLines(t.tail), lines), nil
}

// tailWriter counts the lines written to it and keeps the end of them.
type tailWriter struct {
	tail  []byte
	lines int
	last  byte
}

func (t *tailWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	t.lines += bytes.Count(b, []byte{'\n'})
	t.last = b[len(b)-1]
	t.tail = append(t.tail, b...)
	if len(t.tail) > 256<<10 {
		t.tail = t.tail[len(t.tail)-128<<10:]
	}
	return len(b), nil
}

// lastLines is the last tailLines lines of b.
func lastLines(b []byte) string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "\n")
	return strings.Join(parts[max(0, len(parts)-tailLines):], "\n") + "\n"
}

func describeRun(id string, r jobrun.Result, tail string, lines int) string {
	head := "job " + id + ": "
	switch r.State {
	case jobrun.Lost:
		head += "lost (the VM stopped or rebooted, or its Guest Service was replaced, before it reported its exit)"
	case jobrun.Exited:
		head += fmt.Sprintf("exit code %d", r.Code)
	default:
		head += "still running (exec returned after its timeout; follow it with exec_read, stop it with exec_kill)"
	}
	shown := strings.Count(tail, "\n")
	var count string
	switch {
	case lines == 0:
		count = "no output"
	case lines > shown:
		count = fmt.Sprintf("%d lines of output, last %d below; exec_read from cursor 0 has all of it", lines, shown)
	default:
		count = fmt.Sprintf("%d lines of output", lines)
	}
	if r.Dropped {
		count += "; the start fell out of glovd's 16 MiB window"
	}
	s := head + "\n" + count
	if tail != "" {
		s += "\n---\n" + tail
	}
	return s
}

func execRead(ctx context.Context, c conn, a mcpArgs) (string, error) {
	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 32 << 10
	}
	var r api.ExecRead
	if err := call(ctx, c, api.MethodExecRead, api.ExecReadParams{VM: a.VM, JobID: a.JobID, Cursor: a.Cursor.n, MaxBytes: min(maxBytes, api.MaxReadBytes)}, &r); err != nil {
		return "", err
	}
	head := "running"
	switch {
	case r.Lost:
		head = "lost (the VM stopped or rebooted, or its Guest Service was replaced, before it reported its exit)"
	case r.Exited && r.Code != nil:
		head = fmt.Sprintf("exit code %d", *r.Code)
	case r.Exited:
		head = "exited"
	}
	head += fmt.Sprintf("; cursor %d", r.Cursor)
	if r.Dropped {
		head += "\nthe output before this point fell out of glovd's 16 MiB window"
	}
	if len(r.Data) == 0 {
		return head, nil
	}
	return head + "\n---\n" + string(r.Data), nil
}

// maxTextFile is the most file_read reads, as on macOS.
const maxTextFile = 256 << 10

func fileRead(ctx context.Context, c conn, a mcpArgs) (string, error) {
	limit := maxTextFile
	if a.MaxBytes > 0 {
		limit = min(a.MaxBytes, maxTextFile)
	}
	g := daemonGuest{ctx, c, a.VM}
	var st fileStat
	if err := g.call("file.stat", map[string]any{"path": a.Path}, &st); err != nil {
		return "", err
	}
	switch {
	case !st.Exists:
		return "", fmt.Errorf("%s does not exist in the guest", a.Path)
	case st.IsDir:
		return "", fmt.Errorf("%s is a directory", a.Path)
	case st.Size > int64(limit):
		return "", fmt.Errorf("%s is %d bytes, over %d: use copy_from_vm", a.Path, st.Size, limit)
	}
	var data []byte
	for {
		var r struct {
			Data []byte `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := g.call("file.read", map[string]any{"path": a.Path, "offset": len(data), "length": chunk}, &r); err != nil {
			return "", err
		}
		data = append(data, r.Data...)
		if len(data) > limit {
			return "", fmt.Errorf("%s grew over %d bytes: use copy_from_vm", a.Path, limit)
		}
		if r.EOF || len(r.Data) == 0 {
			break
		}
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not UTF-8 text: use copy_from_vm", a.Path)
	}
	return string(data), nil
}

func fileWrite(ctx context.Context, c conn, a mcpArgs) (string, error) {
	g := daemonGuest{ctx, c, a.VM}
	data := []byte(*a.Text)
	for off := 0; off == 0 || off < len(data); {
		n := min(len(data)-off, chunk)
		p := map[string]any{"path": a.Path, "offset": off, "data": data[off : off+n], "mkdirs": true}
		if off == 0 {
			p["truncate"] = true
		}
		if err := g.call("file.write", p, nil); err != nil {
			return "", err
		}
		off += n
		if n == 0 {
			break
		}
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(data), a.Path), nil
}

func copyTo(ctx context.Context, c conn, a mcpArgs) (string, error) {
	if !filepath.IsAbs(a.HostPath) {
		return "", fmt.Errorf("hostPath %s: want an absolute path", a.HostPath)
	}
	if err := allowCopy(ctx, c, a.VM, a.HostPath, false); err != nil {
		return "", err
	}
	res, err := toGuest(daemonGuest{ctx, c, a.VM}, a.HostPath, a.GuestPath)
	return describeCopy(res, err)
}

func copyFrom(ctx context.Context, c conn, a mcpArgs) (string, error) {
	if !filepath.IsAbs(a.HostPath) {
		return "", fmt.Errorf("hostPath %s: want an absolute path", a.HostPath)
	}
	if err := allowCopy(ctx, c, a.VM, a.HostPath, true); err != nil {
		return "", err
	}
	res, err := fromGuest(daemonGuest{ctx, c, a.VM}, a.GuestPath, a.HostPath)
	return describeCopy(res, err)
}

func describeCopy(res copied, err error) (string, error) {
	var skipped string
	for _, s := range res.skipped {
		skipped += "\nskipped " + s
	}
	if err != nil {
		return "", fmt.Errorf("%v (after %d files, %d bytes)%s", err, res.files, res.bytes, skipped)
	}
	return fmt.Sprintf("%d files, %d bytes", res.files, res.bytes) + skipped, nil
}
