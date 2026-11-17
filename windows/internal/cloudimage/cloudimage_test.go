package cloudimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func TestFindReadsBothListFormats(t *testing.T) {
	s := Source{File: "a.img", Sums: "SUMS", NewHash: sha256.New}
	sum := strings.Repeat("ab", 32)
	for _, list := range []string{
		strings.Repeat("cd", 32) + " *a.img.bak\n" + sum + " *a.img\n",
		sum + "  a.img\n",
	} {
		got, err := s.find([]byte(list))
		if err != nil || got != sum {
			t.Fatalf("find(%q) = %q, %v", list, got, err)
		}
	}
	if _, err := s.find([]byte("abc  a.img\n")); err == nil {
		t.Fatal("a short checksum was accepted")
	}
	if _, err := s.find([]byte(sum + "  b.img\n")); err == nil {
		t.Fatal("found an image the list does not name")
	}
}

func testKey(t *testing.T) (*openpgp.Entity, []byte) {
	t.Helper()
	e, err := openpgp.NewEntity("test", "", "test@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return e, b.Bytes()
}

func sign(t *testing.T, e *openpgp.Entity, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&b, e, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// mirror serves files and counts image downloads.
func mirror(t *testing.T, files map[string][]byte, gets *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/a.img" {
			*gets++
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestLatestAndFetchVerify(t *testing.T) {
	signer, key := testKey(t)
	other, _ := testKey(t)
	image := []byte("qcow2 bytes")
	h := sha256.Sum256(image)
	sum := hex.EncodeToString(h[:])
	sums := []byte(sum + " *a.img\n")
	files := map[string][]byte{"SUMS": sums, "SUMS.gpg": sign(t, signer, sums), "a.img": image}
	gets := 0
	s := Source{Distro: "test", URL: mirror(t, files, &gets), File: "a.img", Sums: "SUMS", NewHash: sha256.New,
		Signature: "SUMS.gpg", Key: key, Fingerprint: strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint))}
	ctx := context.Background()
	img, err := s.Latest(ctx, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if img.Release != "a-"+sum[:12] || img.Sum != sum {
		t.Fatalf("Latest = %+v", img)
	}
	dir := t.TempDir()
	for range 2 {
		path, err := s.Fetch(ctx, http.DefaultClient, img, dir, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, image) || filepath.Base(path) != img.Release+".qcow2" {
			t.Fatalf("Fetch wrote %s: %q", path, got)
		}
	}
	if gets != 1 {
		t.Fatalf("the image was downloaded %d times, want once", gets)
	}

	// A signature by another key is refused.
	files["SUMS.gpg"] = sign(t, other, sums)
	if _, err := s.Latest(ctx, http.DefaultClient); err == nil {
		t.Fatal("a foreign signature was accepted")
	}

	// A download that does not match is refused and not kept.
	files["a.img"] = []byte("other bytes")
	os.RemoveAll(dir)
	if _, err := s.Fetch(ctx, http.DefaultClient, img, dir, func(string) {}); err == nil {
		t.Fatal("a mismatching image was accepted")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}
