// Command glov-image does the disk image work the macOS host used to need
// Homebrew's qemu-img and gpgv for, so Linux guests need nothing from it:
//
//	glov-image convert <image.qcow2> <disk.raw>
//	glov-image verify-ubuntu <SHA256SUMS> <SHA256SUMS.gpg>
//
// convert writes a new sparse raw disk of the image's virtual size; verify-ubuntu
// exits 0 only for a good signature by Ubuntu's cloud image signing key.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/akunzai/glov/diskimage/pgpsig"
	"github.com/akunzai/glov/diskimage/qcow2"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "glov-image:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Println(version)
		return nil
	case len(args) == 3 && args[0] == "convert":
		return convert(args[1], args[2])
	case len(args) == 3 && args[0] == "verify-ubuntu":
		return verifyUbuntu(args[1], args[2])
	}
	return errors.New("usage: glov-image convert <image.qcow2> <disk.raw> | verify-ubuntu <SHA256SUMS> <SHA256SUMS.gpg>")
}

// block is the unit convert leaves as a hole when it reads as zeros, as
// qemu-img does within a cluster: APFS allocates 4 KiB blocks.
const block = 4096

// convert writes src's data to a new file dst, leaving each block that reads
// as zeros as a hole. dst is removed again if anything fails.
func convert(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	img, err := qcow2.Open(in)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	if err := out.Truncate(int64(img.Size())); err != nil {
		return err
	}
	if err := img.Clusters(func(off uint64, data []byte) error {
		// Each run of blocks with data is one write. data need not be a
		// whole number of blocks: small clusters, or the last one cut at
		// the virtual size.
		start := -1
		flush := func(end int) error {
			if start < 0 {
				return nil
			}
			_, err := out.WriteAt(data[start:end], int64(off)+int64(start))
			start = -1
			return err
		}
		for i := 0; i < len(data); i += block {
			if !allZero(data[i:min(i+block, len(data))]) {
				if start < 0 {
					start = i
				}
				continue
			}
			if err := flush(i); err != nil {
				return err
			}
		}
		return flush(len(data))
	}); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	return out.Sync()
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func verifyUbuntu(sums, sig string) error {
	data, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(sig)
	if err != nil {
		return err
	}
	if err := pgpsig.Check(pgpsig.UbuntuKey, pgpsig.UbuntuFingerprint, data, signature); err != nil {
		return fmt.Errorf("%s: %w", sig, err)
	}
	return nil
}
