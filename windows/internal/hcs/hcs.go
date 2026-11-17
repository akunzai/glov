//go:build windows

// Package hcs is a minimal wrapper over the Host Compute System API in computecore.dll,
// ported from the M0 spike.
// Reference: https://learn.microsoft.com/en-us/virtualization/api/hcs/reference/apioverview
package hcs

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	computecore = windows.NewLazySystemDLL("computecore.dll")

	procCreateOperation        = computecore.NewProc("HcsCreateOperation")
	procCloseOperation         = computecore.NewProc("HcsCloseOperation")
	procWaitForOperationResult = computecore.NewProc("HcsWaitForOperationResult")
	procCreateComputeSystem    = computecore.NewProc("HcsCreateComputeSystem")
	procOpenComputeSystem      = computecore.NewProc("HcsOpenComputeSystem")
	procCloseComputeSystem     = computecore.NewProc("HcsCloseComputeSystem")
	procStartComputeSystem     = computecore.NewProc("HcsStartComputeSystem")
	procTerminateComputeSystem = computecore.NewProc("HcsTerminateComputeSystem")
	procShutDownComputeSystem  = computecore.NewProc("HcsShutDownComputeSystem")
	procModifyComputeSystem    = computecore.NewProc("HcsModifyComputeSystem")
	procGetProperties          = computecore.NewProc("HcsGetComputeSystemProperties")
	procEnumerateSystems       = computecore.NewProc("HcsEnumerateComputeSystems")
	procGrantVmAccess          = computecore.NewProc("HcsGrantVmAccess")
	procRevokeVmAccess         = computecore.NewProc("HcsRevokeVmAccess")
	procGetServiceProperties   = computecore.NewProc("HcsGetServiceProperties")
)

const infinite = 0xFFFFFFFF

// System is an open handle to a compute system. Its methods may run
// concurrently with Close; once closed they return ErrClosed rather than
// pass HCS a dead handle.
type System struct {
	ID     string
	mu     sync.RWMutex
	handle uintptr
}

// ErrClosed is returned by a System's methods after Close.
var ErrClosed = errors.New("hcs: compute system handle closed")

// call runs an HCS operation on the open handle.
func (s *System) call(name string, f func(h, op uintptr) uintptr) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.handle == 0 {
		return "", ErrClosed
	}
	return run(name, func(op uintptr) uintptr { return f(s.handle, op) })
}

// Error carries the HRESULT and the result document HCS returned.
type Error struct {
	Op     string
	HR     uint32
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: HRESULT 0x%08X %s", e.Op, e.HR, e.Detail)
}

func utf16(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

func ptr(s string) uintptr { return uintptr(unsafe.Pointer(utf16(s))) }

func failed(r uintptr) bool { return int32(uint32(r)) < 0 }

// run creates an operation, invokes call with it and waits for the result document.
func run(name string, call func(op uintptr) uintptr) (string, error) {
	op, _, _ := procCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("%s: HcsCreateOperation failed", name)
	}
	defer procCloseOperation.Call(op)
	if r := call(op); failed(r) {
		return "", &Error{Op: name, HR: uint32(r)}
	}
	var doc *uint16
	r, _, _ := procWaitForOperationResult.Call(op, infinite, uintptr(unsafe.Pointer(&doc)))
	result := takeString(doc)
	if failed(r) {
		return result, &Error{Op: name, HR: uint32(r), Detail: result}
	}
	return result, nil
}

func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	windows.LocalFree(windows.Handle(unsafe.Pointer(p)))
	return s
}

// ServiceProperties queries the HCS service; it is the cheapest privilege probe.
func ServiceProperties() (string, error) {
	var doc *uint16
	r, _, _ := procGetServiceProperties.Call(ptr(`{"PropertyTypes":["Basic"]}`), uintptr(unsafe.Pointer(&doc)))
	result := takeString(doc)
	if failed(r) {
		return result, &Error{Op: "HcsGetServiceProperties", HR: uint32(r), Detail: result}
	}
	return result, nil
}

// Create creates (but does not start) a compute system from a JSON document.
func Create(id string, config any) (*System, error) {
	doc, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	s := &System{ID: id}
	_, err = run("HcsCreateComputeSystem", func(op uintptr) uintptr {
		r, _, _ := procCreateComputeSystem.Call(ptr(id), ptr(string(doc)), op, 0, uintptr(unsafe.Pointer(&s.handle)))
		return r
	})
	if err != nil {
		if s.handle != 0 {
			s.Close()
		}
		return nil, err
	}
	return s, nil
}

// Open opens an existing compute system by ID.
func Open(id string) (*System, error) {
	s := &System{ID: id}
	r, _, _ := procOpenComputeSystem.Call(ptr(id), uintptr(windows.GENERIC_ALL), uintptr(unsafe.Pointer(&s.handle)))
	if failed(r) {
		return nil, &Error{Op: "HcsOpenComputeSystem", HR: uint32(r)}
	}
	return s, nil
}

func (s *System) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != 0 {
		procCloseComputeSystem.Call(s.handle)
		s.handle = 0
	}
}

func (s *System) Start() error {
	_, err := s.call("HcsStartComputeSystem", func(h, op uintptr) uintptr {
		r, _, _ := procStartComputeSystem.Call(h, op, 0)
		return r
	})
	return err
}

func (s *System) Terminate() error {
	_, err := s.call("HcsTerminateComputeSystem", func(h, op uintptr) uintptr {
		r, _, _ := procTerminateComputeSystem.Call(h, op, 0)
		return r
	})
	return err
}

// Shutdown asks the guest to power off through the Shutdown integration
// service, which the VM's document must declare (schema 2.5); a Linux guest
// answers through hv_utils. It returns once the guest has taken the request.
func (s *System) Shutdown() error {
	_, err := s.call("HcsShutDownComputeSystem", func(h, op uintptr) uintptr {
		r, _, _ := procShutDownComputeSystem.Call(h, op, ptr(`{"Mechanism":"IntegrationService","Type":"Shutdown"}`))
		return r
	})
	return err
}

// Modify applies a ModifySettingRequest document.
func (s *System) Modify(request any) error {
	doc, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = s.call("HcsModifyComputeSystem", func(h, op uintptr) uintptr {
		r, _, _ := procModifyComputeSystem.Call(h, op, ptr(string(doc)), 0)
		return r
	})
	return err
}

// Properties returns the raw properties document for the query (empty for Basic).
func (s *System) Properties(query string) (string, error) {
	if query == "" {
		query = `{"PropertyTypes":[]}`
	}
	return s.call("HcsGetComputeSystemProperties", func(h, op uintptr) uintptr {
		r, _, _ := procGetProperties.Call(h, op, ptr(query))
		return r
	})
}

// RuntimeID returns the VM's GUID, which hv_sock uses as the VM address.
func (s *System) RuntimeID() (string, error) {
	doc, err := s.Properties("")
	if err != nil {
		return "", err
	}
	var p struct{ RuntimeId string }
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return "", fmt.Errorf("properties %q: %w", doc, err)
	}
	return p.RuntimeId, nil
}

// Enumerate lists compute systems matching the query document (empty for all).
func Enumerate(query string) (string, error) {
	return run("HcsEnumerateComputeSystems", func(op uintptr) uintptr {
		r, _, _ := procEnumerateSystems.Call(ptr(query), op)
		return r
	})
}

// GrantVmAccess adds the VM's virtual account to a file's ACL.
func GrantVmAccess(vmID, path string) error {
	r, _, _ := procGrantVmAccess.Call(ptr(vmID), ptr(path))
	if failed(r) {
		return &Error{Op: "HcsGrantVmAccess " + path, HR: uint32(r)}
	}
	return nil
}

// RevokeVmAccess removes what GrantVmAccess added to a file's ACL.
func RevokeVmAccess(vmID, path string) error {
	r, _, _ := procRevokeVmAccess.Call(ptr(vmID), ptr(path))
	if failed(r) {
		return &Error{Op: "HcsRevokeVmAccess " + path, HR: uint32(r)}
	}
	return nil
}
