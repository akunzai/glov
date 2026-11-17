package daemon

import (
	"errors"
	"net"

	"github.com/Microsoft/go-winio"

	"github.com/akunzai/glov/windows/internal/winsec"
)

// hostCallers reads a named pipe client's identity from Windows.
var hostCallers Callers = pipeCallers{}

type pipeCallers struct{}

func (pipeCallers) User(c net.Conn) (string, error) { return winsec.PipeClientSID(c) }

// listenerClosed reports an Accept error from a closed listener.
func listenerClosed(err error) bool {
	return errors.Is(err, winio.ErrPipeListenerClosed) || errors.Is(err, net.ErrClosed)
}
