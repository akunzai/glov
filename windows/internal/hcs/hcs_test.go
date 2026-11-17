//go:build windows

package hcs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// E_ACCESSDENIED and HCS_E_ACCESS_DENIED, which HCS returns to a caller that is
// neither elevated nor allowed, depending on the Windows build.
const (
	accessDenied    = 0x80070005
	hcsAccessDenied = 0x8037011B
)

func TestServicePropertiesReportsSupportedSchemas(t *testing.T) {
	if err := procGetServiceProperties.Find(); err != nil {
		t.Skipf("HCS not available: %v", err)
	}
	doc, err := ServiceProperties()
	if e, ok := errors.AsType[*Error](err); ok && (e.HR == accessDenied || e.HR == hcsAccessDenied) {
		t.Skip("HCS needs an elevated caller")
	}
	if err != nil {
		t.Fatal(err)
	}
	if doc == "" {
		t.Fatal("empty service properties document")
	}
}

func TestErrorCarriesHRESULTAndDetail(t *testing.T) {
	e := &Error{Op: "HcsStartComputeSystem", HR: 0xC03A001A, Detail: "{}"}
	if got, want := e.Error(), "HcsStartComputeSystem: HRESULT 0xC03A001A {}"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestVMSIDMatchesHcsGrantVmAccess(t *testing.T) {
	// HcsGrantVmAccess("6f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c", ...) added
	// this SID on Windows 11 26300.
	id, _ := guid.FromString("6f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c")
	if got, want := VMSID(id), "S-1-5-83-1-1864116894-1267600477-773668762-1817922111"; got != want {
		t.Fatalf("VMSID = %s, want %s", got, want)
	}
}

func TestLinuxVMDocument(t *testing.T) {
	doc := LinuxVM{Owner: "o", User: "S-1-5-21-1-2-3-1001", CPUs: 2, MemoryMiB: 1024, Disk: `C:\d.vhdx`, Seed: `C:\s.vhd`, Console: `\\.\pipe\c`}.Document()
	b, _ := json.Marshal(doc)
	for _, want := range []string{`"Uefi":{`, `"ApplySecureBootTemplate":"Skip"`, `"1":{"Path":"C:\\s.vhd","ReadOnly":true,"Type":"VirtualDisk"}`, `"SizeInMB":1024`, `"NamedPipe":"\\\\.\\pipe\\c"`,
		`"DefaultConnectSecurityDescriptor":"D:P(A;;FA;;;SY)(A;;FA;;;S-1-5-21-1-2-3-1001)"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("document lacks %s:\n%s", want, b)
		}
	}
	b, _ = json.Marshal(LinuxVM{Disk: `C:\d.vhdx`}.Document())
	if strings.Contains(string(b), `"1":`) {
		t.Error("a seed disk without Seed")
	}
	if strings.Contains(string(b), `"NetworkAdapters"`) {
		t.Error("a network adapter")
	}
	if strings.Contains(string(b), `SecureBootTemplateId`) {
		t.Error("a Secure Boot template without SecureBoot")
	}
	b, _ = json.Marshal(LinuxVM{Disk: "d.vhdx", SecureBoot: true}.Document())
	if !strings.Contains(string(b), `"ApplySecureBootTemplate":"Apply"`) || !strings.Contains(string(b), `"SecureBootTemplateId":"`+MicrosoftUEFICATemplate+`"`) {
		t.Errorf("Secure Boot missing: %s", b)
	}
}
