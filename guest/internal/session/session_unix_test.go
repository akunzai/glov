//go:build unix

package session

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestActiveSkipsTheRefFIFOs: logind keeps a "<id>.ref" FIFO beside each
// session record; opening one to read blocks until a writer comes.
func TestActiveSkipsTheRefFIFOs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"1": tty})
	if err := unix.Mkfifo(filepath.Join(dir, "1.ref"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Session, 1)
	go func() {
		s, _ := Active(dir)
		done <- s
	}()
	select {
	case s := <-done:
		if s.User != "carol" {
			t.Errorf("got %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Active blocked on the FIFO")
	}
}
