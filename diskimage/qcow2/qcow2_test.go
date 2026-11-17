package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"strings"
	"testing"
)

const testClusterBits = 9 // 512-byte clusters keep the image small

// build writes a version 3 qcow2 image of size bytes with 512-byte
// clusters: plain holds clusters stored as is, packed compressed ones and
// zeroed ones marked as zero clusters; the rest are unallocated.
func build(t *testing.T, size uint64, plain, packed map[uint64][]byte, zeroed []uint64) []byte {
	t.Helper()
	cs := uint64(1) << testClusterBits
	l2Entries := cs / 8
	clusters := (size + cs - 1) / cs
	l1Size := (clusters + l2Entries - 1) / l2Entries
	var f bytes.Buffer
	f.Write(make([]byte, cs)) // header cluster
	l1Off := uint64(f.Len())
	f.Write(make([]byte, cs))
	l2Off := make([]uint64, l1Size)
	for i := range l2Off {
		l2Off[i] = uint64(f.Len())
		f.Write(make([]byte, cs))
	}
	img := f.Bytes()
	setL2 := func(idx, entry uint64) {
		binary.BigEndian.PutUint64(img[l2Off[idx/l2Entries]+idx%l2Entries*8:], entry)
	}
	for idx, data := range plain {
		off := uint64(len(img))
		img = append(img, data...)
		img = append(img, make([]byte, cs-uint64(len(data)))...)
		setL2(idx, off|1<<63)
	}
	x := 62 - (testClusterBits - 8)
	for idx, data := range packed {
		var z bytes.Buffer
		w, _ := flate.NewWriter(&z, flate.BestCompression)
		w.Write(append(data, make([]byte, cs-uint64(len(data)))...))
		w.Close()
		off := uint64(len(img))
		img = append(img, z.Bytes()...)
		sectors := (off%512 + uint64(z.Len()) + 511) / 512
		setL2(idx, l2Compressed|(sectors-1)<<x|off)
	}
	for _, idx := range zeroed {
		setL2(idx, l2Zero)
	}
	for i, off := range l2Off {
		binary.BigEndian.PutUint64(img[l1Off+uint64(i)*8:], off|1<<63)
	}
	h := img[:112]
	copy(h, Magic)
	binary.BigEndian.PutUint32(h[4:], 3)
	binary.BigEndian.PutUint32(h[20:], testClusterBits)
	binary.BigEndian.PutUint64(h[24:], size)
	binary.BigEndian.PutUint32(h[36:], uint32(l1Size))
	binary.BigEndian.PutUint64(h[40:], l1Off)
	binary.BigEndian.PutUint32(h[96:], 4)
	binary.BigEndian.PutUint32(h[100:], 104)
	return img
}

func read(t *testing.T, img []byte) (map[uint64]string, *Image) {
	t.Helper()
	q, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint64]string{}
	if err := q.Clusters(func(off uint64, data []byte) error {
		got[off] = strings.TrimRight(string(data), "\x00")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got, q
}

func TestClustersReadsPlainCompressedZeroAndUnallocatedClusters(t *testing.T) {
	// 200 clusters span four L2 tables of 64 entries; the last is partial.
	size := uint64(200*512 - 100)
	img := build(t, size,
		map[uint64][]byte{0: []byte("boot"), 70: []byte("plain"), 3: make([]byte, 10)},
		map[uint64][]byte{1: []byte(strings.Repeat("packed", 40)), 199: []byte("last")},
		[]uint64{2})
	got, q := read(t, img)
	if q.Size() != size {
		t.Errorf("size %d", q.Size())
	}
	want := map[uint64]string{0: "boot", 512: strings.Repeat("packed", 40), 70 * 512: "plain", 199 * 512: "last"}
	if len(got) != len(want) {
		t.Errorf("clusters %v", got)
	}
	for off, s := range want {
		if got[off] != s {
			t.Errorf("cluster at %d = %q, want %q", off, got[off], s)
		}
	}
}

func TestOpenRefusesWhatItCannotRead(t *testing.T) {
	good := build(t, 4096, nil, nil, nil)
	for name, mutate := range map[string]func(h []byte){
		"magic":      func(h []byte) { h[0] = 'X' },
		"version":    func(h []byte) { binary.BigEndian.PutUint32(h[4:], 4) },
		"backing":    func(h []byte) { binary.BigEndian.PutUint64(h[8:], 512) },
		"encryption": func(h []byte) { binary.BigEndian.PutUint32(h[32:], 1) },
		"cluster":    func(h []byte) { binary.BigEndian.PutUint32(h[20:], 30) },
		"corrupt":    func(h []byte) { binary.BigEndian.PutUint64(h[72:], incompatCorrupt) },
		"extended":   func(h []byte) { binary.BigEndian.PutUint64(h[72:], incompatExtL2) },
		"zstd": func(h []byte) {
			binary.BigEndian.PutUint64(h[72:], incompatCompType)
			binary.BigEndian.PutUint32(h[100:], 112)
			h[104] = 1
		},
		"l1": func(h []byte) { binary.BigEndian.PutUint64(h[24:], 1<<40) },
	} {
		img := bytes.Clone(good)
		mutate(img)
		if _, err := Open(bytes.NewReader(img)); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := Open(bytes.NewReader(good)); err != nil {
		t.Errorf("good image: %v", err)
	}
}
