package harness

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// The owner's harness config on the host (Profile.HostConfig) is copied
// into an isolated VM one way, as plain files: links and junctions are
// followed, so a skill linked from elsewhere arrives as a real directory,
// and a broken link or a cycle is left out and named. Credentials are never
// copied: a file of such a name inside a copied directory is left out.

// CredentialNames hold sign-ins or tokens, wherever they are.
var CredentialNames = []string{".credentials.json", "auth.json", "antigravity-oauth-token", "oauth_creds.json", "credentials.json", ".env"}

// The most a copy holds, so a stray tree cannot fill the guest.
const (
	MaxHostConfigBytes = 64 << 20
	MaxHostConfigFiles = 10_000
)

// Copied is one host config copy StageHostConfig made.
type Copied struct {
	Guest        string
	Files, Bytes int
}

// HostConfigReport is what StageHostConfig did: the copies made, and what
// it left out, with why.
type HostConfigReport struct {
	Copied  []Copied
	Skipped []string
}

// HostCopies is the profiles' HostConfig, each guest path once.
func HostCopies(profiles []Profile) []HostCopy {
	var out []HostCopy
	for _, p := range profiles {
		for _, c := range p.HostConfig {
			if !slices.ContainsFunc(out, func(o HostCopy) bool { return o.Guest == c.Guest }) {
				out = append(out, c)
			}
		}
	}
	return out
}

// StageHostConfig copies each of copies that exists below hostHome into
// staging/<guest path>, links followed. It fails past MaxHostConfigBytes
// or MaxHostConfigFiles in all.
func StageHostConfig(copies []HostCopy, hostHome, staging string) (HostConfigReport, error) {
	var rep HostConfigReport
	files, bytes := 0, 0
	for _, c := range copies {
		source := filepath.Join(hostHome, filepath.FromSlash(c.Host))
		if _, err := os.Lstat(source); err != nil {
			continue
		}
		cf, cb := 0, 0
		// seen holds the real paths of the directories above, so a link
		// back up is a cycle.
		var copyTree func(from, to, shown string, seen []string) error
		copyTree = func(from, to, shown string, seen []string) error {
			real, err := filepath.EvalSymlinks(from)
			if err != nil {
				rep.Skipped = append(rep.Skipped, "~/"+shown+" (broken link)")
				return nil
			}
			fi, err := os.Stat(real)
			if err != nil {
				rep.Skipped = append(rep.Skipped, "~/"+shown+" (broken link)")
				return nil
			}
			if slices.Contains(CredentialNames, path.Base(shown)) {
				rep.Skipped = append(rep.Skipped, "~/"+shown+" (credentials)")
				return nil
			}
			if fi.IsDir() {
				if slices.Contains(seen, real) {
					rep.Skipped = append(rep.Skipped, "~/"+shown+" (link cycle)")
					return nil
				}
				if err := os.MkdirAll(to, 0o700); err != nil {
					return err
				}
				entries, err := os.ReadDir(real)
				if err != nil {
					return err
				}
				for _, e := range entries { // sorted by name
					if err := copyTree(filepath.Join(real, e.Name()), filepath.Join(to, e.Name()), shown+"/"+e.Name(), append(slices.Clone(seen), real)); err != nil {
						return err
					}
				}
				return nil
			}
			if !fi.Mode().IsRegular() {
				rep.Skipped = append(rep.Skipped, "~/"+shown+" (not a file)")
				return nil
			}
			files++
			bytes += int(fi.Size())
			if files > MaxHostConfigFiles || bytes > MaxHostConfigBytes {
				return fmt.Errorf("the host config is over %d files or %d MiB at ~/%s; copy less, or pass --no-host-config",
					MaxHostConfigFiles, MaxHostConfigBytes>>20, shown)
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				return err
			}
			if err := copyFile(real, to); err != nil {
				return err
			}
			cf++
			cb += int(fi.Size())
			return nil
		}
		if err := copyTree(source, filepath.Join(staging, filepath.FromSlash(c.Guest)), c.Host, nil); err != nil {
			return rep, err
		}
		if cf > 0 {
			rep.Copied = append(rep.Copied, Copied{Guest: c.Guest, Files: cf, Bytes: cb})
		}
	}
	return rep, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s is copied twice", to)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
