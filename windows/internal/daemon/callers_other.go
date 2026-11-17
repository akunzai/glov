//go:build !windows

package daemon

import (
	"errors"
	"net"
)

// hostCallers knows no caller off Windows, so a Daemon without Callers
// serves nobody there.
var hostCallers Callers = noCallers{}

type noCallers struct{}

var errNoCaller = errors.New("no caller identity on this host")

func (noCallers) User(net.Conn) (string, error) { return "", errNoCaller }

func (noCallers) Process(net.Conn) (string, error) { return "", errNoCaller }

// listenerClosed reports an Accept error from a closed listener.
func listenerClosed(err error) bool { return errors.Is(err, net.ErrClosed) }
