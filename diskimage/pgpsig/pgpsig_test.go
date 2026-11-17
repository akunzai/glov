package pgpsig

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func TestRealUbuntuSignatureVerifies(t *testing.T) {
	sums, err := os.ReadFile("testdata/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile("testdata/SHA256SUMS.gpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(UbuntuKey, UbuntuFingerprint, sums, sig); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(sums, []byte("5"), []byte("6"), 1)
	if err := Check(UbuntuKey, UbuntuFingerprint, tampered, sig); err == nil {
		t.Fatal("a changed checksum list verified")
	}
	if err := Check(UbuntuKey, UbuntuFingerprint, sums, sig[:len(sig)/2]); err == nil {
		t.Fatal("a cut signature verified")
	}
}

func TestBinarySignaturesAndOtherKeys(t *testing.T) {
	e, err := openpgp.NewEntity("test", "", "test@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var key bytes.Buffer
	w, err := armor.Encode(&key, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	data := []byte("abc  a.img\n")
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, e, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	if err := Check(key.Bytes(), fmt.Sprintf("%X", e.PrimaryKey.Fingerprint), data, sig.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := Check(key.Bytes(), UbuntuFingerprint, data, sig.Bytes()); err == nil {
		t.Fatal("a signature by another key passed for Ubuntu's fingerprint")
	}
	if err := Check(UbuntuKey, UbuntuFingerprint, data, sig.Bytes()); err == nil {
		t.Fatal("a signature by an unknown key verified")
	}
}
