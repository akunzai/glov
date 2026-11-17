package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type swapFixture struct {
	src, dst string
	events   []string
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	dir := t.TempDir()
	f := &swapFixture{src: filepath.Join(dir, "new.exe"), dst: filepath.Join(dir, "installed.exe")}
	must(t, os.WriteFile(f.src, []byte("new"), 0o755))
	must(t, os.WriteFile(f.dst, []byte("old"), 0o755))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *swapFixture) content(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.dst)
	must(t, err)
	return string(b)
}

func (f *swapFixture) step(name string, err error) func() error {
	return func() error { f.events = append(f.events, name); return err }
}

func (f *swapFixture) assertEvents(t *testing.T, want ...string) {
	t.Helper()
	if len(f.events) != len(want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
	for i := range want {
		if f.events[i] != want[i] {
			t.Fatalf("events = %v, want %v", f.events, want)
		}
	}
}

func TestSwapReplacesBinaryBetweenStopAndStart(t *testing.T) {
	f := newSwapFixture(t)
	must(t, swapBinary(f.src, f.dst, f.step("stop", nil), f.step("start", nil)))
	if got := f.content(t); got != "new" {
		t.Fatalf("installed = %q", got)
	}
	f.assertEvents(t, "stop", "start")
	for _, left := range []string{f.dst + ".new", f.dst + ".old"} {
		if _, err := os.Stat(left); err == nil {
			t.Fatalf("%s left behind", left)
		}
	}
}

func TestSwapFailedStopLeavesOldBinaryAndNeverStarts(t *testing.T) {
	f := newSwapFixture(t)
	err := swapBinary(f.src, f.dst, f.step("stop", errors.New("busy")), f.step("start", nil))
	if err == nil {
		t.Fatal("want error")
	}
	if got := f.content(t); got != "old" {
		t.Fatalf("installed = %q", got)
	}
	f.assertEvents(t, "stop")
}

func TestSwapFailedStartRestoresOldBinaryAndStartsIt(t *testing.T) {
	f := newSwapFixture(t)
	calls := 0
	start := func() error {
		calls++
		f.events = append(f.events, "start")
		if calls == 1 {
			return errors.New("does not start")
		}
		return nil
	}
	if err := swapBinary(f.src, f.dst, f.step("stop", nil), start); err == nil {
		t.Fatal("want error")
	}
	if got := f.content(t); got != "old" {
		t.Fatalf("installed = %q, want the old Guest Service back", got)
	}
	f.assertEvents(t, "stop", "start", "stop", "start")
}

func TestSwapMissingSourceTouchesNothing(t *testing.T) {
	f := newSwapFixture(t)
	if err := swapBinary(filepath.Join(filepath.Dir(f.src), "nope.exe"), f.dst, f.step("stop", nil), f.step("start", nil)); err == nil {
		t.Fatal("want error")
	}
	f.assertEvents(t)
	if got := f.content(t); got != "old" {
		t.Fatalf("installed = %q", got)
	}
}

func TestSwapWhenNothingInstalledYetStillStarts(t *testing.T) {
	f := newSwapFixture(t)
	must(t, os.Remove(f.dst))
	must(t, swapBinary(f.src, f.dst, f.step("stop", nil), f.step("start", nil)))
	if got := f.content(t); got != "new" {
		t.Fatalf("installed = %q", got)
	}
}
