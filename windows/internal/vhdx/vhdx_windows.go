// Package vhdx creates the VHDX files a VM boots from, with Windows' own
// virtual disk API (virtdisk.dll): a base disk converted from a cloud image,
// and a differencing disk per VM on top of it.
package vhdx

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/diskimage/qcow2"
	"github.com/akunzai/glov/windows/internal/seed"
)

// BlockSize is the VHDX block size. 1 MiB, as WSL uses for ext4 disks, keeps
// a differencing disk from growing by 32 MiB (the default) per small write.
const BlockSize = 1 << 20

var (
	virtdisk              = windows.NewLazySystemDLL("virtdisk.dll")
	procResizeVirtualDisk = virtdisk.NewProc("ResizeVirtualDisk")
	procMergeVirtualDisk  = virtdisk.NewProc("MergeVirtualDisk")
)

// FromVHD writes a dynamic VHDX at dst holding src's data, with src's virtual
// size. It writes dst.part first, so a failed conversion leaves no dst
// behind.
func FromVHD(src, dst string) error {
	part := dst + ".part.vhdx" // virtdisk picks the format by extension
	os.Remove(part)
	h, err := vhd.CreateVirtualDisk(part, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone, &vhd.CreateVirtualDiskParameters{
		Version: 2,
		Version2: vhd.CreateVersion2{
			SourcePath:       windows.StringToUTF16Ptr(src),
			BlockSizeInBytes: BlockSize,
		},
	})
	if err != nil {
		os.Remove(part)
		return fmt.Errorf("convert %s to VHDX: %w", src, err)
	}
	syscall.CloseHandle(h)
	return os.Rename(part, dst)
}

// Differencing creates child as a differencing VHDX on parent, grown to size
// bytes; the space beyond parent's size stays unallocated until the guest
// grows its root partition into it. The child records parent's path, so
// parent must not move afterwards.
func Differencing(child, parent string, size uint64) error {
	if _, err := os.Stat(child); err == nil {
		return fmt.Errorf("%s exists", child)
	}
	if err := vhd.CreateDiffVhd(child, parent, BlockSize>>20); err != nil {
		os.Remove(child)
		return err
	}
	if err := Resize(child, size); err != nil {
		os.Remove(child)
		return err
	}
	return nil
}

// Resize grows the disk at path to size bytes; virtdisk refuses to shrink it.
// The disk must not be attached to a running VM.
func Resize(path string, size uint64) error {
	h, err := vhd.OpenVirtualDiskWithParameters(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone, &vhd.OpenVirtualDiskParameters{Version: 2})
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	params := struct {
		Version uint32 // RESIZE_VIRTUAL_DISK_VERSION_1
		_       uint32
		NewSize uint64
	}{Version: 1, NewSize: size}
	if r, _, _ := procResizeVirtualDisk.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&params)), 0); r != 0 {
		return fmt.Errorf("resize %s to %d MiB: %w", path, size>>20, syscall.Errno(r))
	}
	return nil
}

// Merge writes the differencing disk at child into its parent, which then
// holds what child held; child is left behind, no longer needed, for the
// caller to delete. Neither may be attached to a running VM, and parent
// must be at least as large as child.
func Merge(child string) error {
	h, err := vhd.OpenVirtualDiskWithParameters(child, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone, &vhd.OpenVirtualDiskParameters{Version: 2})
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	params := struct {
		Version          uint32 // MERGE_VIRTUAL_DISK_VERSION_2
		MergeSourceDepth uint32 // child itself
		MergeTargetDepth uint32 // its parent
	}{Version: 2, MergeSourceDepth: 1, MergeTargetDepth: 2}
	if r, _, _ := procMergeVirtualDisk.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&params)), 0); r != 0 {
		return fmt.Errorf("merge %s into its parent: %w", child, syscall.Errno(r))
	}
	return nil
}

// FromQCOW2 writes a dynamic VHDX at dst holding the qcow2 image at src's
// data. It expands src into a fixed VHD next to dst first, converts that, and
// removes it. The fixed VHD is not sparse: virtdisk refuses sparse sources.
func FromQCOW2(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, err := qcow2.Open(f)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if img.Size()%512 != 0 {
		return fmt.Errorf("%s: virtual size %d is not a multiple of 512", src, img.Size())
	}
	raw := dst + ".part.vhd"
	os.Remove(raw)
	defer os.Remove(raw)
	out, err := os.Create(raw)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := img.Clusters(func(off uint64, data []byte) error {
		_, err := out.WriteAt(data, int64(off))
		return err
	}); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if _, err := out.WriteAt(seed.FixedVHDFooter(img.Size(), time.Now()), int64(img.Size())); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return FromVHD(raw, dst)
}
