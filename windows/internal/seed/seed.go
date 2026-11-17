// Package seed writes a NoCloud seed disk: a fixed VHD holding one FAT16 file
// system (no partition table) labelled CIDATA, with files in its root
// directory. cloud-init finds it by that label and runs its user-data on
// the VM's first boot.
package seed

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf16"
)

// Label is the volume label NoCloud looks for.
const Label = "CIDATA"

// File is one file in the seed's root directory.
type File struct {
	Name string
	Data []byte
}

const (
	sector      = 512
	rootEntries = 512
	maxClusters = 65524 // FAT16's limit
	minClusters = 4085  // fewer would make it FAT12
)

// Write creates the seed disk at path. It fails if path exists.
func Write(path string, files []File) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	err = write(w, files)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

type layout struct {
	spc, fatSectors, clusters int64
}

func (l layout) clusterSize() int64 { return l.spc * sector }
func (l layout) rootSectors() int64 { return rootEntries * 32 / sector }
func (l layout) dataStart() int64   { return 1 + 2*l.fatSectors + l.rootSectors() }
func (l layout) total() int64       { return l.dataStart() + l.clusters*l.spc }

// plan picks the smallest cluster size whose FAT16 holds the files, with a
// little room to spare.
func plan(files []File) (layout, error) {
	for spc := int64(1); spc <= 64; spc *= 2 {
		cs := spc * sector
		var need int64
		for _, f := range files {
			need += (int64(len(f.Data)) + cs - 1) / cs
		}
		clusters := max(need+16, minClusters+16)
		if clusters > maxClusters {
			continue
		}
		fat := ((clusters+2)*2 + sector - 1) / sector
		return layout{spc: spc, fatSectors: fat, clusters: clusters}, nil
	}
	return layout{}, errors.New("seed: files too large for a FAT16 seed disk")
}

func write(w io.Writer, files []File) error {
	l, err := plan(files)
	if err != nil {
		return err
	}
	dir, err := directory(files, l)
	if err != nil {
		return err
	}
	fat := make([]byte, l.fatSectors*sector)
	binary.LittleEndian.PutUint16(fat[0:], 0xFFF8)
	binary.LittleEndian.PutUint16(fat[2:], 0xFFFF)
	next := uint16(2)
	for _, f := range files {
		n := (int64(len(f.Data)) + l.clusterSize() - 1) / l.clusterSize()
		for i := range n {
			v := next + uint16(i) + 1
			if i == n-1 {
				v = 0xFFFF
			}
			binary.LittleEndian.PutUint16(fat[(int64(next)+i)*2:], v)
		}
		next += uint16(n)
	}

	out := &counter{w: w}
	out.Write(bootSector(l))
	out.Write(fat)
	out.Write(fat)
	out.Write(dir)
	for _, f := range files {
		out.Write(f.Data)
		if pad := int64(len(f.Data)) % l.clusterSize(); pad != 0 {
			out.Write(make([]byte, l.clusterSize()-pad))
		}
	}
	// Zero the free clusters too: a fixed VHD's size is in its footer.
	for out.n < l.total()*sector && out.err == nil {
		out.Write(make([]byte, min(1<<20, l.total()*sector-out.n)))
	}
	out.Write(FixedVHDFooter(uint64(l.total()*sector), time.Now()))
	return out.err
}

type counter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *counter) Write(b []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
}

func bootSector(l layout) []byte {
	b := make([]byte, sector)
	copy(b, []byte{0xEB, 0x3C, 0x90})
	copy(b[3:], "GLOV")
	binary.LittleEndian.PutUint16(b[11:], sector)
	b[13] = byte(l.spc)
	binary.LittleEndian.PutUint16(b[14:], 1) // reserved sectors
	b[16] = 2                                // FATs
	binary.LittleEndian.PutUint16(b[17:], rootEntries)
	if l.total() < 1<<16 {
		binary.LittleEndian.PutUint16(b[19:], uint16(l.total()))
	} else {
		binary.LittleEndian.PutUint32(b[32:], uint32(l.total()))
	}
	b[21] = 0xF8 // fixed disk
	binary.LittleEndian.PutUint16(b[22:], uint16(l.fatSectors))
	binary.LittleEndian.PutUint16(b[24:], 63)  // sectors per track
	binary.LittleEndian.PutUint16(b[26:], 255) // heads
	b[36] = 0x80                               // drive number
	b[38] = 0x29                               // extended boot signature
	rand.Read(b[39:43])                        // volume serial number
	copy(b[43:54], pad11(Label))
	copy(b[54:62], "FAT16   ")
	b[510], b[511] = 0x55, 0xAA
	return b
}

func pad11(s string) []byte {
	b := []byte("           ")
	copy(b, s)
	return b
}

// directory returns the root directory: the volume label, then each file
// as long-name entries followed by a generated 8.3 entry (SEED0001...).
func directory(files []File, l layout) ([]byte, error) {
	dir := make([]byte, rootEntries*32)
	e := 0
	add := func(entry []byte) error {
		if e >= rootEntries {
			return errors.New("seed: too many files for the root directory")
		}
		copy(dir[e*32:], entry)
		e++
		return nil
	}
	label := make([]byte, 32)
	copy(label, pad11(Label))
	label[11] = 0x08
	add(label)
	cluster := uint16(2)
	for i, f := range files {
		name := utf16.Encode([]rune(f.Name))
		if len(f.Name) == 0 || len(name) > 255 {
			return nil, fmt.Errorf("seed: bad file name %q", f.Name)
		}
		short := pad11(fmt.Sprintf("SEED%04d", i+1))
		sum := checksum(short)
		parts := (len(name) + 12) / 13
		for p := parts; p >= 1; p-- {
			ent := make([]byte, 32)
			ord := byte(p)
			if p == parts {
				ord |= 0x40
			}
			ent[0] = ord
			ent[11] = 0x0F
			ent[13] = sum
			chars := make([]uint16, 13)
			for j := range chars {
				k := (p-1)*13 + j
				switch {
				case k < len(name):
					chars[j] = name[k]
				case k == len(name):
					chars[j] = 0
				default:
					chars[j] = 0xFFFF
				}
			}
			for j, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				binary.LittleEndian.PutUint16(ent[off:], chars[j])
			}
			if err := add(ent); err != nil {
				return nil, err
			}
		}
		ent := make([]byte, 32)
		copy(ent, short)
		ent[11] = 0x20                                  // archive
		binary.LittleEndian.PutUint16(ent[18:], 0x0021) // 1980-01-01
		binary.LittleEndian.PutUint16(ent[24:], 0x0021)
		if len(f.Data) > 0 {
			binary.LittleEndian.PutUint16(ent[26:], cluster)
			cluster += uint16((int64(len(f.Data)) + l.clusterSize() - 1) / l.clusterSize())
		}
		binary.LittleEndian.PutUint32(ent[28:], uint32(len(f.Data)))
		if err := add(ent); err != nil {
			return nil, err
		}
	}
	return dir, nil
}

func checksum(short []byte) byte {
	var s byte
	for _, c := range short {
		s = (s>>1 | s<<7) + c
	}
	return s
}

// FixedVHDFooter is a fixed VHD footer for a disk of size bytes (VHD specification
// 1.0, "Hard Disk Footer Format").
func FixedVHDFooter(size uint64, now time.Time) []byte {
	f := make([]byte, sector)
	copy(f, "conectix")
	binary.BigEndian.PutUint32(f[8:], 2)           // features: reserved bit
	binary.BigEndian.PutUint32(f[12:], 0x00010000) // format version
	binary.BigEndian.PutUint64(f[16:], ^uint64(0)) // no dynamic header
	binary.BigEndian.PutUint32(f[24:], uint32(now.Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))/time.Second))
	copy(f[28:], "glov")
	binary.BigEndian.PutUint32(f[32:], 0x00010000)
	copy(f[36:], "Wi2k")
	binary.BigEndian.PutUint64(f[40:], size)
	binary.BigEndian.PutUint64(f[48:], size)
	c, h, s := geometry(size / sector)
	binary.BigEndian.PutUint16(f[56:], c)
	f[58], f[59] = h, s
	binary.BigEndian.PutUint32(f[60:], 2) // fixed
	rand.Read(f[68:84])                   // unique ID
	var sum uint32
	for _, b := range f {
		sum += uint32(b)
	}
	binary.BigEndian.PutUint32(f[64:], ^sum)
	return f
}

// geometry is the specification's CHS calculation for a disk of n sectors.
func geometry(n uint64) (cyl uint16, heads, spt uint8) {
	n = min(n, 65535*16*255)
	var cth uint64
	var h, s uint64
	if n >= 65535*16*63 {
		s, h = 255, 16
		cth = n / s
	} else {
		s = 17
		cth = n / s
		h = max((cth+1023)/1024, 4)
		if cth >= h*1024 || h > 16 {
			s, h = 31, 16
			cth = n / s
		}
		if cth >= h*1024 {
			s, h = 63, 16
			cth = n / s
		}
	}
	return uint16(cth / h), uint8(h), uint8(s)
}
