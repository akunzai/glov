package main

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
)

// fakeGlovd answers glovd methods from handlers and records calls.
type fakeGlovd struct {
	mu       sync.Mutex
	calls    []string
	params   map[string]string
	handlers map[string]func(params string) (any, error)
}

func (f *fakeGlovd) Call(_ context.Context, method string, params, result any) error {
	b, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.params[method] = string(b)
	h := f.handlers[method]
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	v, err := h(string(b))
	if err != nil || result == nil {
		return err
	}
	out, _ := json.Marshal(v)
	return json.Unmarshal(out, result)
}

func (f *fakeGlovd) Close() error { return nil }

func mcpSession(t *testing.T, f *fakeGlovd) *mcp.ClientSession {
	t.Helper()
	if f.params == nil {
		f.params = map[string]string{}
	}
	s := &mcpServer{dial: func(context.Context) (conn, error) { return f, nil }}
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.server().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

// The server announced a hard-coded 0.1.0 after the 0.2.0 release.
func TestMCPAnnouncesTheBuildVersion(t *testing.T) {
	old := version
	version = "9.8.7+test"
	t.Cleanup(func() { version = old })
	cs := mcpSession(t, &fakeGlovd{})
	if got := cs.InitializeResult().ServerInfo; got.Name != "glov" || got.Version != "9.8.7+test" {
		t.Errorf("server info = %+v", got)
	}
}

func TestMCPListsTheToolsThisEditionHas(t *testing.T) {
	cs := mcpSession(t, &fakeGlovd{})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		schema, _ := json.Marshal(tool.InputSchema)
		if tool.Name != "vm_list" && !strings.Contains(string(schema), `"description":"VM name or unique name prefix (see vm_list)."`) {
			t.Errorf("%s schema lacks vm: %s", tool.Name, schema)
		}
	}
	slices.Sort(names)
	want := []string{"copy_from_vm", "copy_to_vm", "egress_allow", "egress_list", "egress_log", "egress_remove",
		"exec", "exec_kill", "exec_read", "exec_start", "file_read", "file_write", "share_sync", "vm_configure", "vm_list", "vm_reboot", "vm_start", "vm_status", "vm_stop", "vm_wait"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestMCPMapsArgumentsToGlovd(t *testing.T) {
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodVMConfigure: func(string) (any, error) { return api.VMInfo{Name: "dev", State: "stopped", Engine: "hcs"}, nil },
	}}
	cs := mcpSession(t, f)
	out, isErr := callTool(t, cs, "vm_configure", map[string]any{"vm": "dev", "memory": "8G", "diskSize": "64G"})
	if isErr || !strings.Contains(out, `"state":"stopped"`) || f.params[api.MethodVMConfigure] != `{"vm":"dev","cpus":0,"memoryMiB":8192,"diskGiB":64}` {
		t.Errorf("vm_configure = %q (error %v), sent %s", out, isErr, f.params[api.MethodVMConfigure])
	}
	if out, isErr := callTool(t, cs, "vm_configure", map[string]any{"vm": "dev"}); !isErr || !strings.Contains(out, "at least one") {
		t.Errorf("an empty vm_configure = %q", out)
	}
	if out, isErr := callTool(t, cs, "exec", map[string]any{"vm": "dev"}); !isErr || !strings.Contains(out, "needs argv") {
		t.Errorf("exec without argv = %q", out)
	}
}

func TestMCPExecReturnsTheExitCodeAndTheLastLines(t *testing.T) {
	var out strings.Builder
	for i := range 60 {
		out.WriteString("line " + string(rune('a'+i%26)) + "\n")
	}
	code := 3
	reads := 0
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodExecStart: func(string) (any, error) { return api.JobStarted{JobID: "j1"}, nil },
		api.MethodExecRead: func(string) (any, error) {
			reads++
			if reads > 1 {
				return api.ExecRead{Cursor: len(out.String()), Exited: true, Code: &code}, nil
			}
			return api.ExecRead{Data: []byte(out.String()), Cursor: len(out.String())}, nil
		},
	}}
	cs := mcpSession(t, f)
	got, isErr := callTool(t, cs, "exec", map[string]any{"vm": "dev", "argv": []string{"/bin/sh", "-c", "x"}})
	if isErr || !strings.HasPrefix(got, "job j1: exit code 3\n60 lines of output, last 50 below") || strings.Count(got, "line ") != 50 {
		t.Errorf("exec = %q", got)
	}
	if !strings.Contains(f.params[api.MethodExecStart], `"argv":["/bin/sh","-c","x"]`) {
		t.Errorf("exec.start params %s", f.params[api.MethodExecStart])
	}
}

func TestMCPFileReadRefusesLargeAndBinaryFiles(t *testing.T) {
	size, data := 5, []byte("hello")
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodGuestCall: func(p string) (any, error) {
			if strings.Contains(p, `"method":"file.stat"`) {
				return fileStat{Exists: true, Size: int64(size)}, nil
			}
			return map[string]any{"data": data, "eof": true}, nil
		},
	}}
	cs := mcpSession(t, f)
	if got, isErr := callTool(t, cs, "file_read", map[string]any{"vm": "dev", "path": "/etc/x"}); isErr || got != "hello" {
		t.Errorf("file_read = %q", got)
	}
	data = []byte{0xff, 0xfe, 0, 1, 2}
	if got, isErr := callTool(t, cs, "file_read", map[string]any{"vm": "dev", "path": "/bin/x"}); !isErr || !strings.Contains(got, "not UTF-8") {
		t.Errorf("a binary file_read = %q", got)
	}
	size = 300 << 10
	if got, isErr := callTool(t, cs, "file_read", map[string]any{"vm": "dev", "path": "/var/big"}); !isErr || !strings.Contains(got, "copy_from_vm") {
		t.Errorf("a large file_read = %q", got)
	}
}

func TestMCPCopyRefusesHostPathsOutsideTheAllowlist(t *testing.T) {
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodShareList: func(string) (any, error) { return []api.ShareInfo{}, nil },
	}}
	cs := mcpSession(t, f)
	if got, isErr := callTool(t, cs, "copy_to_vm", map[string]any{"vm": "dev", "hostPath": t.TempDir(), "guestPath": "/root/x"}); !isErr || !strings.Contains(got, "outside VM dev's copy allowlist") {
		t.Errorf("copy_to_vm = %q", got)
	}
	if got, isErr := callTool(t, cs, "copy_from_vm", map[string]any{"vm": "dev", "hostPath": "relative", "guestPath": "/root/x"}); !isErr || !strings.Contains(got, "absolute") {
		t.Errorf("copy_from_vm = %q", got)
	}
	if slices.Contains(f.calls, api.MethodGuestCall) {
		t.Error("a refused copy reached the guest")
	}
}

func TestMCPExecReadReportsTheCursorAndExit(t *testing.T) {
	code := 0
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodExecRead: func(string) (any, error) {
			return api.ExecRead{Data: []byte("tick 3\n"), Cursor: 21, Exited: true, Code: &code, Dropped: true}, nil
		},
	}}
	cs := mcpSession(t, f)
	got, isErr := callTool(t, cs, "exec_read", map[string]any{"vm": "dev", "jobId": "j1", "cursor": 14})
	if isErr || got != "exit code 0; cursor 21\nthe output before this point fell out of glovd's 16 MiB window\n---\ntick 3\n" {
		t.Errorf("exec_read = %q", got)
	}
	if p := f.params[api.MethodExecRead]; p != `{"vm":"dev","jobId":"j1","cursor":14,"maxBytes":32768}` {
		t.Errorf("exec.read params %s", p)
	}
	if got, isErr := callTool(t, cs, "exec_kill", map[string]any{"vm": "dev", "jobId": "j1"}); isErr || got != "job j1 killed" {
		t.Errorf("exec_kill = %q", got)
	}
}

func TestMCPEgressChangesAreMarkedAsComingFromMCP(t *testing.T) {
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodEgressAllow: func(string) (any, error) {
			return api.Egress{VM: "box", Allow: []string{"api.example.com", "*.ubuntu.com"}}, nil
		},
		api.MethodEgressList: func(string) (any, error) { return api.Egress{VM: "box", Allow: []string{}}, nil },
	}}
	cs := mcpSession(t, f)
	out, isErr := callTool(t, cs, "egress_allow", map[string]any{"vm": "box", "hosts": []string{"api.example.com", "*.ubuntu.com"}, "mcp": false})
	if isErr || out != "box allows: api.example.com, *.ubuntu.com" || f.params[api.MethodEgressAllow] != `{"vm":"box","hosts":["api.example.com","*.ubuntu.com"],"mcp":true}` {
		t.Errorf("egress_allow = %q (error %v), sent %s", out, isErr, f.params[api.MethodEgressAllow])
	}
	callTool(t, cs, "egress_remove", map[string]any{"vm": "box", "hosts": []string{"x.com"}})
	if p := f.params[api.MethodEgressRemove]; p != `{"vm":"box","hosts":["x.com"],"mcp":true}` {
		t.Errorf("egress_remove sent %s", p)
	}
	if out, isErr := callTool(t, cs, "egress_allow", map[string]any{"vm": "box"}); !isErr || !strings.Contains(out, "needs hosts") {
		t.Errorf("egress_allow without hosts = %q", out)
	}
	if out, _ := callTool(t, cs, "egress_list", map[string]any{"vm": "box"}); out != "box: nothing allowed (every connection is denied)" {
		t.Errorf("egress_list = %q", out)
	}
}

func TestMCPEgressLogPassesTheCursorBack(t *testing.T) {
	host, port := "b.com", 443
	f := &fakeGlovd{handlers: map[string]func(string) (any, error){
		api.MethodEgressLog: func(params string) (any, error) {
			if strings.Contains(params, `"cursor"`) {
				return egress.Page{Records: []egress.Record{}, Cursor: "7:200"}, nil
			}
			return egress.Page{Records: []egress.Record{{Time: "2026-11-01T00:00:00Z", VM: "box", Decision: egress.Deny, Host: &host, Port: &port, Reason: "not in the allowlist"}}, Cursor: "7:100"}, nil
		},
	}}
	cs := mcpSession(t, f)
	out, _ := callTool(t, cs, "egress_log", map[string]any{"vm": "box", "denied": true})
	if out != "2026-11-01T00:00:00Z deny b.com:443 in=0 out=0 (not in the allowlist)\ncursor: 7:100" || f.params[api.MethodEgressLog] != `{"vm":"box","denied":true}` {
		t.Errorf("egress_log = %q, sent %s", out, f.params[api.MethodEgressLog])
	}
	out, _ = callTool(t, cs, "egress_log", map[string]any{"vm": "box", "cursor": "7:100"})
	if out != "no connections\ncursor: 7:200" || f.params[api.MethodEgressLog] != `{"vm":"box","cursor":"7:100"}` {
		t.Errorf("egress_log after the cursor = %q, sent %s", out, f.params[api.MethodEgressLog])
	}
}
