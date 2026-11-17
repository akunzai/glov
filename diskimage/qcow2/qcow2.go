// Package qcow2 reads the qcow2 disk images Debian and Ubuntu publish their
// cloud images as, so either host can turn them into a disk without
// qemu-img. It reads what those images use: version 2 or 3, no backing file,
// no encryption, no external data file, zlib-compressed and plain clusters,
// zero clusters, and no extended L2 entries; anything else is refused.
package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic starts every qcow2 file.
const Magic = "QFI\xfb"

const (
	offsetMask       = 0x00fffffffffffe00 // L1 and standard L2 entries
	l2Compressed     = 1 << 62
	l2Zero           = 1 // standard cluster reads as zeros (version 3)
	incompatCorrupt  = 1 << 1
	incompatExternal = 1 << 2
	incompatCompType = 1 << 3
	incompatExtL2    = 1 << 4
	knownIncompat    = 1<<0 | incompatCorrupt | incompatExternal | incompatCompType | incompatExtL2
	// maxClusterBits bounds the cluster size at 2 MiB, qcow2's own limit.
	minClusterBits = 9
	maxClusterBits = 21
)

// Image is an opened qcow2 image.
type Image struct {
	r           io.ReaderAt
	size        uint64 // the virtual disk size
	clusterBits uint
	l1          []uint64
}

// Size is the virtual disk size in bytes.
func (img *Image) Size() uint64 { return img.size }

// ClusterSize is the size of one cluster in bytes.
func (img *Image) ClusterSize() int { return 1 << img.clusterBits }

// Open reads r's header and L1 table.
func Open(r io.ReaderAt) (*Image, error) {
	h := make([]byte, 112)
	if _, err := r.ReadAt(h[:72], 0); err != nil {
		return nil, fmt.Errorf("qcow2 header: %w", err)
	}
	if string(h[:4]) != Magic {
		return nil, errors.New("not a qcow2 image")
	}
	be := binary.BigEndian
	version := be.Uint32(h[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("qcow2 version %d is not supported", version)
	}
	if be.Uint64(h[8:]) != 0 {
		return nil, errors.New("qcow2 images with a backing file are not supported")
	}
	clusterBits := be.Uint32(h[20:])
	if clusterBits < minClusterBits || clusterBits > maxClusterBits {
		return nil, fmt.Errorf("qcow2 cluster size 2^%d is not supported", clusterBits)
	}
	if be.Uint32(h[32:]) != 0 {
		return nil, errors.New("encrypted qcow2 images are not supported")
	}
	if version == 3 {
		if _, err := r.ReadAt(h[72:104], 72); err != nil {
			return nil, fmt.Errorf("qcow2 header: %w", err)
		}
		incompat := be.Uint64(h[72:])
		if incompat&^knownIncompat != 0 || incompat&(incompatCorrupt|incompatExternal|incompatExtL2) != 0 {
			return nil, fmt.Errorf("qcow2 incompatible features %#x are not supported", incompat)
		}
		if incompat&incompatCompType != 0 {
			// The compression type byte follows a header of at least 105 bytes.
			if be.Uint32(h[100:]) < 105 {
				return nil, errors.New("qcow2 header too short for its compression type")
			}
			if _, err := r.ReadAt(h[104:105], 104); err != nil {
				return nil, fmt.Errorf("qcow2 header: %w", err)
			}
			if h[104] != 0 {
				return nil, fmt.Errorf("qcow2 compression type %d is not supported (only zlib)", h[104])
			}
		}
	}
	img := &Image{r: r, size: be.Uint64(h[24:]), clusterBits: uint(clusterBits)}
	l1Size := uint64(be.Uint32(h[36:]))
	l2Entries := uint64(1) << (clusterBits - 3)
	clusters := (img.size + uint64(img.ClusterSize()) - 1) >> clusterBits
	if need := (clusters + l2Entries - 1) / l2Entries; l1Size < need {
		return nil, fmt.Errorf("qcow2 L1 table has %d entries, the disk needs %d", l1Size, need)
	} else if l1Size > need+1 || l1Size > 1<<24 {
		return nil, fmt.Errorf("qcow2 L1 table of %d entries is implausible", l1Size)
	}
	raw := make([]byte, l1Size*8)
	if _, err := r.ReadAt(raw, int64(be.Uint64(h[40:]))); err != nil {
		return nil, fmt.Errorf("qcow2 L1 table: %w", err)
	}
	img.l1 = make([]uint64, l1Size)
	for i := range img.l1 {
		img.l1[i] = be.Uint64(raw[i*8:])
	}
	return img, nil
}

// Clusters calls fn with each cluster's guest offset and data in order,
// skipping clusters that read as zeros; data is reused between calls.
func (img *Image) Clusters(fn func(offset uint64, data []byte) error) error {
	cs := uint64(img.ClusterSize())
	l2Entries := cs / 8
	l2 := make([]byte, cs)
	data := make([]byte, cs)
	compressed := make([]byte, 2*cs)
	for i, l1e := range img.l1 {
		l2Off := l1e & offsetMask
		if l2Off == 0 {
			continue
		}
		if _, err := img.r.ReadAt(l2, int64(l2Off)); err != nil {
			return fmt.Errorf("qcow2 L2 table %d: %w", i, err)
		}
		for j := uint64(0); j < l2Entries; j++ {
			guest := (uint64(i)*l2Entries + j) * cs
			if guest >= img.size {
				return nil
			}
			e := binary.BigEndian.Uint64(l2[j*8:])
			var err error
			switch {
			case e&l2Compressed != 0:
				err = img.readCompressed(e, compressed, data)
			case e&l2Zero != 0 || e&offsetMask == 0:
				continue
			default:
				_, err = img.r.ReadAt(data, int64(e&offsetMask))
			}
			if err != nil {
				return fmt.Errorf("qcow2 cluster at %d: %w", guest, err)
			}
			n := min(cs, img.size-guest)
			if allZero(data[:n]) {
				continue
			}
			if err := fn(guest, data[:n]); err != nil {
				return err
			}
		}
	}
	return nil
}

// readCompressed inflates the compressed cluster entry e into data.
func (img *Image) readCompressed(e uint64, buf, data []byte) error {
	x := 62 - (img.clusterBits - 8)
	off := e & (1<<x - 1)
	sectors := (e>>x)&(1<<(62-x)-1) + 1
	n := sectors*512 - off&511
	if n > uint64(len(buf)) {
		return fmt.Errorf("compressed cluster of %d bytes is implausible", n)
	}
	// The last cluster may end at the file's end, before n bytes.
	m, err := img.r.ReadAt(buf[:n], int64(off))
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	zr := flate.NewReader(bytes.NewReader(buf[:m]))
	defer zr.Close()
	if _, err := io.ReadFull(zr, data); err != nil {
		return fmt.Errorf("inflate: %w", err)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
