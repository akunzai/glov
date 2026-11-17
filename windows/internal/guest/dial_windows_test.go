package guest

import (
	"testing"

	"github.com/Microsoft/go-winio"
)

// docs/guest-protocol.md names this service ID; keep the two in step.
func TestGuestServiceIDMatchesProtocolDoc(t *testing.T) {
	if got, want := winio.VsockServiceID(Port).String(), "00000fb6-facb-11e6-bd58-64006a7986d3"; got != want {
		t.Fatalf("service ID = %s, want %s", got, want)
	}
}
