// Package pgpsig checks the detached OpenPGP signatures distributions put on
// their checksum lists, against one pinned key, so neither host needs gpgv.
package pgpsig

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// UbuntuKey is the UEC Image Automatic Signing Key, which signs Ubuntu's
// cloud image checksums, from keyserver.ubuntu.com.
//
//go:embed ubuntu-cloud.asc
var UbuntuKey []byte

// UbuntuFingerprint is UbuntuKey's primary key fingerprint, from Ubuntu's
// "Verify an image checksum" guide,
// https://documentation.ubuntu.com/public-images/public-images-how-to/verify-image-checksum/
// (checked 2026-10-25). Check also requires it of the signer, so a replaced
// key block cannot pass.
const UbuntuFingerprint = "D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81"

// Check returns nil when sig, armored or binary, is a good signature over
// data by the armored key whose primary fingerprint is fingerprint.
func Check(key []byte, fingerprint string, data, sig []byte) error {
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	check := openpgp.CheckDetachedSignature
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP SIGNATURE-----")) {
		check = openpgp.CheckArmoredDetachedSignature
	}
	signer, err := check(keys, bytes.NewReader(data), bytes.NewReader(sig), nil)
	if err != nil {
		return err
	}
	if fp := fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint); fp != fingerprint {
		return fmt.Errorf("signed by %s, not %s", fp, fingerprint)
	}
	return nil
}
