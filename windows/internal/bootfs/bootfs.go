// Package bootfs reads files out of a disk image's EFI system partition: a
// fixed VHD holding a GPT disk whose ESP is FAT16 or FAT32, or a bare FAT
// volume such as the seed disk, which the seed tests read back with it.
//
// The image is verified before it gets here, but the reader still treats it
// as untrusted input: every offset, chain and size is bounds-checked.
package bootfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// MaxFile bounds a file read out of the image.
const MaxFile = 256 << 20

const sector = 512

// FixedVHD returns the disk data of a fixed VHD: everything but the 512-byte
// footer, after checking that the footer says "fixed".
func FixedVHD(r io.ReaderAt, size int64) (*io.SectionReader, error) {
	if size < 2*sector {
		return nil, errors.New("bootfs: file too small for a VHD")
	}
	var f [sector]byte
	if _, err := r.ReadAt(f[:], size-sector); err != nil {
		return nil, err
	}
	if string(f[:8]) != "conectix" {
		return nil, errors.New("bootfs: no VHD footer")
	}
	if t := binary.BigEndian.Uint32(f[60:]); t != 2 {
		return nil, fmt.Errorf("bootfs: VHD disk type %d, want 2 (fixed)", t)
	}
	if cur := binary.BigEndian.Uint64(f[48:]); cur != uint64(size-sector) {
		return nil, fmt.Errorf("bootfs: VHD footer size %d, file holds %d", cur, size-sector)
	}
	return io.NewSectionReader(r, 0, size-sector), nil
}

// espType is the EFI system partition type GUID, C12A7328-F81F-11D2-BA4B-00A0C93EC93B,
// in GPT's mixed-endian byte order.
var espType = []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}

// ESP finds the EFI system partition of a GPT disk with 512-byte sectors.
func ESP(disk *io.SectionReader) (*io.SectionReader, error) {
	var h [92]byte
	if _, err := disk.ReadAt(h[:], sector); err != nil {
		return nil, fmt.Errorf("bootfs: GPT header: %w", err)
	}
	if string(h[:8]) != "EFI PART" {
		return nil, errors.New("bootfs: no GPT")
	}
	entryLBA := binary.LittleEndian.Uint64(h[72:])
	count := binary.LittleEndian.Uint32(h[80:])
	entrySize := binary.LittleEndian.Uint32(h[84:])
	if entrySize < 128 || entrySize > 4096 || count > 1024 || entryLBA > uint64(disk.Size())/sector {
		return nil, errors.New("bootfs: implausible GPT header")
	}
	e := make([]byte, entrySize)
	for i := range count {
		if _, err := disk.ReadAt(e, int64(entryLBA)*sector+int64(i)*int64(entrySize)); err != nil {
			return nil, fmt.Errorf("bootfs: GPT entry %d: %w", i, err)
		}
		if !bytes.Equal(e[:16], espType) {
			continue
		}
		first, last := binary.LittleEndian.Uint64(e[32:]), binary.LittleEndian.Uint64(e[40:])
		if first == 0 || last < first || last >= uint64(disk.Size())/sector {
			return nil, fmt.Errorf("bootfs: ESP spans sectors %d-%d, outside the disk", first, last)
		}
		return io.NewSectionReader(disk, int64(first)*sector, int64(last-first+1)*sector), nil
	}
	return nil, errors.New("bootfs: no EFI system partition")
}

// FS is a read-only FAT16 or FAT32 file system.
type FS struct {
	r              io.ReaderAt
	bytesPerSector int64
	clusterSize    int64
	fatOffset      int64
	dataOffset     int64
	clusters       uint32 // data clusters, numbered 2..clusters+1
	fat32          bool
	rootOffset     int64  // FAT16: fixed root directory
	rootSize       int64  // FAT16
	rootCluster    uint32 // FAT32
}

// OpenFAT reads a FAT16 or FAT32 boot sector.
func OpenFAT(r *io.SectionReader) (*FS, error) {
	var b [sector]byte
	if _, err := r.ReadAt(b[:], 0); err != nil {
		return nil, err
	}
	if b[510] != 0x55 || b[511] != 0xAA {
		return nil, errors.New("bootfs: no FAT boot sector")
	}
	bps := int64(binary.LittleEndian.Uint16(b[11:]))
	spc := int64(b[13])
	reserved := int64(binary.LittleEndian.Uint16(b[14:]))
	fats := int64(b[16])
	rootEntries := int64(binary.LittleEndian.Uint16(b[17:]))
	total := int64(binary.LittleEndian.Uint16(b[19:]))
	if total == 0 {
		total = int64(binary.LittleEndian.Uint32(b[32:]))
	}
	fatSize := int64(binary.LittleEndian.Uint16(b[22:]))
	if fatSize == 0 {
		fatSize = int64(binary.LittleEndian.Uint32(b[36:]))
	}
	if bps < 512 || bps > 4096 || bps&(bps-1) != 0 || spc == 0 || spc&(spc-1) != 0 || reserved == 0 || fats == 0 || fatSize == 0 {
		return nil, errors.New("bootfs: implausible FAT boot sector")
	}
	rootSectors := (rootEntries*32 + bps - 1) / bps
	dataStart := reserved + fats*fatSize + rootSectors
	if total <= dataStart || total*bps > r.Size() {
		return nil, errors.New("bootfs: FAT larger than its partition")
	}
	fs := &FS{
		r:              r,
		bytesPerSector: bps,
		clusterSize:    spc * bps,
		fatOffset:      reserved * bps,
		dataOffset:     dataStart * bps,
		clusters:       uint32((total - dataStart) / spc),
	}
	switch {
	case fs.clusters < 4085:
		return nil, errors.New("bootfs: FAT12 is not supported")
	case fs.clusters < 65525:
		fs.rootOffset = (reserved + fats*fatSize) * bps
		fs.rootSize = rootSectors * bps
	default:
		fs.fat32 = true
		fs.rootCluster = binary.LittleEndian.Uint32(b[44:]) & 0x0FFFFFFF
		if !fs.valid(fs.rootCluster) {
			return nil, errors.New("bootfs: FAT32 root cluster out of range")
		}
	}
	return fs, nil
}

func (fs *FS) valid(c uint32) bool { return c >= 2 && c < fs.clusters+2 }

// next returns the cluster after c in its chain, or 0 at the end.
func (fs *FS) next(c uint32) (uint32, error) {
	var n uint32
	if fs.fat32 {
		var b [4]byte
		if _, err := fs.r.ReadAt(b[:], fs.fatOffset+int64(c)*4); err != nil {
			return 0, err
		}
		n = binary.LittleEndian.Uint32(b[:]) & 0x0FFFFFFF
		if n >= 0x0FFFFFF8 {
			return 0, nil
		}
	} else {
		var b [2]byte
		if _, err := fs.r.ReadAt(b[:], fs.fatOffset+int64(c)*2); err != nil {
			return 0, err
		}
		n = uint32(binary.LittleEndian.Uint16(b[:]))
		if n >= 0xFFF8 {
			return 0, nil
		}
	}
	if !fs.valid(n) {
		return 0, fmt.Errorf("bootfs: cluster chain points to %#x", n)
	}
	return n, nil
}

// chain reads up to limit bytes following the cluster chain from first.
func (fs *FS) chain(first uint32, limit int64) ([]byte, error) {
	if !fs.valid(first) {
		return nil, fmt.Errorf("bootfs: first cluster %#x out of range", first)
	}
	var out []byte
	seen := map[uint32]bool{}
	for c := first; c != 0 && int64(len(out)) < limit; {
		if seen[c] {
			return nil, fmt.Errorf("bootfs: cluster chain loops back to %#x", c)
		}
		seen[c] = true
		buf := make([]byte, fs.clusterSize)
		if _, err := fs.r.ReadAt(buf, fs.dataOffset+int64(c-2)*fs.clusterSize); err != nil {
			return nil, err
		}
		out = append(out, buf...)
		var err error
		if c, err = fs.next(c); err != nil {
			return nil, err
		}
	}
	if int64(len(out)) < limit {
		return nil, errors.New("bootfs: cluster chain shorter than the file")
	}
	return out[:limit], nil
}

type entry struct {
	name    string
	dir     bool
	cluster uint32
	size    int64
}

func (fs *FS) root() ([]byte, error) {
	if fs.fat32 {
		// A directory has no size: read its whole chain, bounded.
		return fs.dirChain(fs.rootCluster)
	}
	b := make([]byte, fs.rootSize)
	_, err := fs.r.ReadAt(b, fs.rootOffset)
	return b, err
}

func (fs *FS) dirChain(first uint32) ([]byte, error) {
	const maxDir = 4 << 20
	var out []byte
	seen := map[uint32]bool{}
	for c := first; c != 0; {
		if !fs.valid(c) || seen[c] || int64(len(out)) >= maxDir {
			return nil, errors.New("bootfs: directory chain out of range, looping or too long")
		}
		seen[c] = true
		buf := make([]byte, fs.clusterSize)
		if _, err := fs.r.ReadAt(buf, fs.dataOffset+int64(c-2)*fs.clusterSize); err != nil {
			return nil, err
		}
		out = append(out, buf...)
		var err error
		if c, err = fs.next(c); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// entries parses a directory, joining long file names to their entries.
func entries(dir []byte) []entry {
	var out []entry
	var lfn []uint16
	var lfnSum byte
	for i := 0; i+32 <= len(dir); i += 32 {
		d := dir[i : i+32]
		switch {
		case d[0] == 0x00:
			return out
		case d[0] == 0xE5:
			lfn = nil
			continue
		case d[11] == 0x0F: // long-name fragment, stored last first
			if d[0]&0x40 != 0 {
				lfn = nil
			}
			var part []uint16
			for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
				for j := r[0]; j < r[1]; j += 2 {
					part = append(part, binary.LittleEndian.Uint16(d[j:]))
				}
			}
			lfn = append(part, lfn...)
			lfnSum = d[13]
			continue
		case d[11]&0x08 != 0: // volume label
			lfn = nil
			continue
		}
		name := shortName(d[:11])
		if lfn != nil && checksum(d[:11]) == lfnSum {
			if end := indexOf(lfn, 0); end >= 0 {
				lfn = lfn[:end]
			}
			name = string(utf16.Decode(lfn))
		}
		lfn = nil
		cluster := uint32(binary.LittleEndian.Uint16(d[20:]))<<16 | uint32(binary.LittleEndian.Uint16(d[26:]))
		out = append(out, entry{name: name, dir: d[11]&0x10 != 0, cluster: cluster, size: int64(binary.LittleEndian.Uint32(d[28:]))})
	}
	return out
}

func indexOf(s []uint16, v uint16) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func shortName(n []byte) string {
	base := strings.TrimRight(string(n[:8]), " ")
	ext := strings.TrimRight(string(n[8:11]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func checksum(n []byte) byte {
	var s byte
	for _, c := range n {
		s = (s>>1 | s<<7) + c
	}
	return s
}

// ReadFile returns a file by its slash-separated path, matched without regard
// to case as FAT does.
func (fs *FS) ReadFile(path string) ([]byte, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	dir, err := fs.root()
	if err != nil {
		return nil, err
	}
	for i, p := range parts {
		var found *entry
		for _, e := range entries(dir) {
			if strings.EqualFold(e.name, p) {
				found = &e
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("bootfs: %s not found", path)
		}
		if i < len(parts)-1 {
			if !found.dir {
				return nil, fmt.Errorf("bootfs: %s is not a directory", p)
			}
			if dir, err = fs.dirChain(found.cluster); err != nil {
				return nil, err
			}
			continue
		}
		if found.dir {
			return nil, fmt.Errorf("bootfs: %s is a directory", path)
		}
		if found.size > MaxFile {
			return nil, fmt.Errorf("bootfs: %s is %d bytes, over the %d limit", path, found.size, MaxFile)
		}
		if found.size == 0 {
			return []byte{}, nil
		}
		return fs.chain(found.cluster, found.size)
	}
	return nil, fmt.Errorf("bootfs: %s not found", path)
}
