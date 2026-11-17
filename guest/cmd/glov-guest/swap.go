package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// swapBinary replaces the file at dst with the one at src while the service
// that runs dst is stopped, then starts it again. A running Windows service
// holds its executable open, so the replacement happens between stop and
// start. If anything fails after the stop, the old binary is put back and the
// service is started again, so a failed update never leaves a working Guest Service
// stopped.
func swapBinary(src, dst string, stop, start func() error) (err error) {
	next, old := dst+".new", dst+".old"
	if err := copyFile(src, next); err != nil {
		return fmt.Errorf("stage new Guest Service: %w", err)
	}
	defer os.Remove(next)
	if err := stop(); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	// From here the service is stopped: every failure path must start it again.
	hadOld := false
	if err := os.Rename(dst, old); err == nil {
		hadOld = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("move old Guest Service aside: %w", err), start())
	}
	if err := os.Rename(next, dst); err != nil {
		return errors.Join(fmt.Errorf("put new Guest Service in place: %w", err), restore(old, dst, hadOld), start())
	}
	if err := start(); err != nil {
		// A failed start can leave the replacement pending and its file open.
		if stopErr := stop(); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		return errors.Join(fmt.Errorf("start new Guest Service: %w", err), restore(old, dst, hadOld), start())
	}
	os.Remove(old)
	return nil
}

func restore(old, dst string, hadOld bool) error {
	if !hadOld {
		return nil
	}
	return os.Rename(old, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
