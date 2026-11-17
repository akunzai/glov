// Package cloudimage finds, downloads and verifies Debian's and Ubuntu's
// official x86_64 cloud images (qcow2), pinned to one major release and the
// newest build within it (decision 2 in akunzai/glov#146).
//
// Each publisher keeps its newest build in one directory under a fixed name,
// so a build is told apart by its checksum: the release name ends in the
// checksum's first hex digits.
package cloudimage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/akunzai/glov/diskimage/pgpsig"
	"github.com/akunzai/glov/windows/internal/fetch"
	"github.com/akunzai/glov/windows/internal/progress"
)

// Source is where one distribution's image is published.
type Source struct {
	Distro string
	// URL is the directory holding the newest build and its checksums.
	URL string
	// File is the image's name there.
	File string
	// Sums names the checksum list in URL, hashed with NewHash.
	Sums    string
	NewHash func() hash.Hash
	// Signature names Sums' detached GPG signature, made by the key whose
	// fingerprint is Fingerprint in Key. Empty when the publisher does not
	// sign it (Debian): then only HTTPS ties the list to the publisher.
	Signature   string
	Key         []byte
	Fingerprint string
}

// Debian 13 (trixie)'s generic cloud image.
var Debian = Source{
	Distro:  "debian",
	URL:     "https://cloud.debian.org/images/cloud/trixie/latest",
	File:    "debian-13-genericcloud-amd64.qcow2",
	Sums:    "SHA512SUMS",
	NewHash: sha512.New,
}

// Ubuntu 26.04 LTS (resolute)'s server cloud image, a qcow2 despite its
// .img name.
var Ubuntu = Source{
	Distro:      "ubuntu",
	URL:         "https://cloud-images.ubuntu.com/releases/resolute/release",
	File:        "ubuntu-26.04-server-cloudimg-amd64.img",
	Sums:        "SHA256SUMS",
	NewHash:     sha256.New,
	Signature:   "SHA256SUMS.gpg",
	Key:         pgpsig.UbuntuKey,
	Fingerprint: pgpsig.UbuntuFingerprint,
}

// Image is one build of a Source's image.
type Image struct {
	// Release is the image's name without extension and the first 12 hex
	// digits of its checksum, e.g. debian-13-genericcloud-amd64-3ace18e33bdd.
	Release string
	// Sum is the published checksum, lower-case hex.
	Sum string
}

// Latest reads the published checksum list, checks its signature where the
// publisher signs it, and returns the image's current build.
func (s Source) Latest(ctx context.Context, c *http.Client) (Image, error) {
	sums, err := fetch.Get(ctx, c, s.URL+"/"+s.Sums, 1<<20)
	if err != nil {
		return Image{}, err
	}
	if s.Signature != "" {
		sig, err := fetch.Get(ctx, c, s.URL+"/"+s.Signature, 64<<10)
		if err != nil {
			return Image{}, err
		}
		if err := s.checkSignature(sums, sig); err != nil {
			return Image{}, err
		}
	}
	sum, err := s.find(sums)
	if err != nil {
		return Image{}, err
	}
	return Image{Release: strings.TrimSuffix(s.File, filepath.Ext(s.File)) + "-" + sum[:12], Sum: sum}, nil
}

// find returns s.File's checksum in a list of "<hex>  <name>" or
// "<hex> *<name>" lines.
func (s Source) find(sums []byte) (string, error) {
	size := s.NewHash().Size() * 2
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		sum, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || strings.TrimLeft(name, " *") != s.File {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != size {
			return "", fmt.Errorf("%s: unreadable checksum for %s: %q", s.Sums, s.File, sum)
		}
		return strings.ToLower(sum), nil
	}
	return "", fmt.Errorf("%s/%s does not list %s", s.URL, s.Sums, s.File)
}

func (s Source) checkSignature(sums, sig []byte) error {
	if err := pgpsig.Check(s.Key, s.Fingerprint, sums, sig); err != nil {
		return fmt.Errorf("%s/%s: %w", s.URL, s.Signature, err)
	}
	return nil
}

// Fetch makes sure dir holds img, matching its checksum, and returns its
// path. progress receives download and checksum progress lines.
func (s Source) Fetch(ctx context.Context, c *http.Client, img Image, dir string, progress func(string)) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, img.Release+".qcow2")
	if _, err := os.Stat(path); err == nil {
		if err := s.verify(path, img, progress); err == nil {
			return path, nil
		}
		os.Remove(path) // damaged: download it again
	}
	part := path + ".part"
	if err := fetch.Download(ctx, c, s.URL+"/"+s.File, part, progress); err != nil {
		return "", err
	}
	if err := s.verify(part, img, progress); err != nil {
		os.Remove(part)
		return "", err
	}
	if err := os.Rename(part, path); err != nil {
		return "", err
	}
	return path, nil
}

func (s Source) verify(path string, img Image, report func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := s.NewHash()
	if err := progress.WhileRunning("verifying "+s.File, report, progress.Every, func() error {
		_, err := io.Copy(h, f)
		return err
	}); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != img.Sum {
		return fmt.Errorf("%s: checksum %s does not match the published %s", s.File, got, img.Sum)
	}
	return nil
}

// ErrUnknown is returned by ByDistro for a distribution without a Source.
var ErrUnknown = errors.New("no cloud image source")

// ByDistro returns the Source for "debian" or "ubuntu".
func ByDistro(distro string) (Source, error) {
	switch distro {
	case Debian.Distro:
		return Debian, nil
	case Ubuntu.Distro:
		return Ubuntu, nil
	}
	return Source{}, fmt.Errorf("%w for %q", ErrUnknown, distro)
}
