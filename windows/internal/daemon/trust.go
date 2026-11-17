package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/store"
)

// trustDirectory is where the guest keeps glov vm trust certificates;
// update-ca-certificates reads subdirectories too.
const trustDirectory = "/usr/local/share/ca-certificates/glov-trust"

// trustTimeout bounds one trust sync in the guest.
const trustTimeout = 60 * time.Second

// isTrustName reports whether name may name a certificate: it becomes the
// guest file <name>.crt.
func isTrustName(name string) bool {
	if len(name) < 1 || len(name) > 64 || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsFunc(name, func(c rune) bool {
		return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-')
	})
}

// trustedCertificate is text as one certificate named name, re-encoded so
// the guest gets nothing but base64 between the PEM lines.
func trustedCertificate(name, text string) (egress.Trust, error) {
	if !isTrustName(name) {
		return egress.Trust{}, fmt.Errorf("trust name %q: use A-Z a-z 0-9 . _ - (1 to 64 characters)", name)
	}
	switch n := strings.Count(text, "-----BEGIN CERTIFICATE-----"); n {
	case 0:
		return egress.Trust{}, errors.New("no PEM certificate (-----BEGIN CERTIFICATE-----) in the file")
	case 1:
	default:
		return egress.Trust{}, fmt.Errorf("%d certificates in the file: add one CA per name", n)
	}
	rest := []byte(text)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return egress.Trust{}, errors.New("not a readable certificate: the PEM block does not decode")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return egress.Trust{}, fmt.Errorf("not a readable certificate: %v", err)
		}
		return egress.Trust{Name: name, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))}, nil
	}
}

// trustSummary is what glov vm trust list shows of t.
func trustSummary(t egress.Trust) api.TrustedCertificate {
	out := api.TrustedCertificate{Name: t.Name, Subject: "(unreadable)"}
	block, _ := pem.Decode([]byte(t.Certificate))
	if block == nil {
		return out
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return out
	}
	sum := sha256.Sum256(block.Bytes)
	notAfter := c.NotAfter.UTC()
	out.Subject, out.NotAfter, out.SHA256, out.Authority = c.Subject.String(), &notAfter, hex.EncodeToString(sum[:]), c.BasicConstraintsValid && c.IsCA
	return out
}

func trustOf(r store.Record) api.Trust {
	res := api.Trust{VM: r.Name, Certificates: []api.TrustedCertificate{}}
	if r.Egress != nil {
		for _, t := range r.Egress.Trust {
			res.Certificates = append(res.Certificates, trustSummary(t))
		}
	}
	return res
}

func (d *Daemon) trustList(caller string, p api.TrustParams) (api.Trust, error) {
	r, err := d.isolated(caller, p.VM)
	if err != nil {
		return api.Trust{}, err
	}
	return trustOf(r), nil
}

// trustEdit adds (or replaces) or removes one of the VM's trusted
// certificates, then brings a running guest's trust store up to date.
func (d *Daemon) trustEdit(caller string, p api.TrustParams, add bool) (api.Trust, error) {
	if p.Name == "" {
		return api.Trust{}, proto.Errorf(proto.CodeInvalidParams, "name: name the certificate")
	}
	var added egress.Trust
	if add {
		if p.Certificate == "" {
			return api.Trust{}, proto.Errorf(proto.CodeInvalidParams, "certificate: give the PEM text")
		}
		var err error
		if added, err = trustedCertificate(p.Name, p.Certificate); err != nil {
			return api.Trust{}, err
		}
	}
	e, err := d.editEgress(caller, p.VM, func(name string, e *store.Egress) error {
		list := slices.Clone(e.Trust)
		i := slices.IndexFunc(list, func(t egress.Trust) bool { return t.Name == p.Name })
		switch {
		case add && i >= 0:
			list[i] = added
		case add:
			list = append(list, added)
		case i < 0:
			return fmt.Errorf("VM %s has no trusted certificate named %s: see glov vm trust list %s", name, p.Name, name)
		default:
			list = slices.Delete(list, i, i+1)
		}
		if len(list) == 0 {
			list = nil
		}
		e.Trust = list
		return nil
	})
	if err != nil {
		return api.Trust{}, err
	}
	if err := d.syncTrust(caller, e.VM); err != nil {
		return api.Trust{}, fmt.Errorf("saved, but %v; the next Guest Service session retries", err)
	}
	return d.trustList(caller, api.TrustParams{VM: e.VM})
}

// trustScript writes the certificates to a temporary directory and swaps it
// in only when it differs. Names and PEM text are safe to embed:
// trustedCertificate allows only [A-Za-z0-9._-] names and re-encodes the PEM.
func trustScript(list []egress.Trust) string {
	var b strings.Builder
	b.WriteString("set -e\nd=" + trustDirectory + "; t=$(mktemp -d)\n")
	for _, t := range list {
		fmt.Fprintf(&b, "cat > \"$t/%s.crt\" <<'GLOV_PEM'\n%sGLOV_PEM\n", t.Name, t.Certificate)
	}
	b.WriteString(`if [ -z "$(ls -A "$t")" ] && [ ! -e "$d" ]; then rm -rf "$t"; exit 0; fi
if [ -d "$d" ] && diff -r -q "$t" "$d" >/dev/null 2>&1; then rm -rf "$t"; exit 0; fi
rm -rf "$d"
if [ -n "$(ls -A "$t")" ]; then chmod 755 "$t"; chmod 644 "$t"/*; mv "$t" "$d"; else rm -rf "$t"; fi
update-ca-certificates --fresh >/dev/null
echo changed
`)
	return b.String()
}

// syncTrust makes the running VM's trust store hold exactly its trusted
// certificates, one sync per VM at a time; a stopped VM is left for its
// next Guest Service session.
func (d *Daemon) syncTrust(owner, name string) error {
	m, _ := d.trustSyncs.LoadOrStore(name, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	defer m.(*sync.Mutex).Unlock()
	r, err := d.Store.Load(owner, name)
	if err != nil || !r.Isolated || r.ID == "" || !d.VMs.Running(r.ID) {
		return nil
	}
	var list []egress.Trust
	if r.Egress != nil {
		list = r.Egress.Trust
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustTimeout)
	defer cancel()
	var out []string
	code, err := d.runGuest(ctx, r, []string{"sh", "-c", trustScript(list)}, func(line string) { out = append(out, line) })
	if err == nil && code != 0 {
		err = fmt.Errorf("exit code %d: %s", code, strings.Join(out[max(0, len(out)-3):], "; "))
	}
	if err != nil {
		slog.Warn("cannot update the trust store", "vm", name, "err", err)
		return fmt.Errorf("cannot update the trust store: %v", err)
	}
	if slices.Contains(out, "changed") {
		names := []string{}
		for _, t := range list {
			names = append(names, t.Name)
		}
		slog.Info("trusted certificates synced", "vm", name, "certificates", names)
	}
	return nil
}
