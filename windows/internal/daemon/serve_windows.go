package daemon

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

// PipeSDDL is the control pipe's security descriptor: owned by and fully
// accessible to pipeOwner, the account glovd runs as (SYSTEM for the
// service, so glov can tell it from an impostor), and read/write for the
// owner user only. 0x12019b is FILE_GENERIC_READ|FILE_GENERIC_WRITE without
// FILE_CREATE_PIPE_INSTANCE, so the user cannot add server instances.
func PipeSDDL(pipeOwner, user string) string {
	return fmt.Sprintf("O:%sD:P(A;;GA;;;%s)(A;;0x12019b;;;%s)", pipeOwner, pipeOwner, user)
}

// Listen creates the control pipe at path.
func (d *Daemon) Listen(path, pipeOwner string) (net.Listener, error) {
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: PipeSDDL(pipeOwner, d.Owner)})
}
