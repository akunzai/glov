package daemon

import "net"

// Callers tells who is on the other end of a connection glovd serves:
// only the owner is served.
type Callers interface {
	// User is the SID a control connection's client presents.
	User(c net.Conn) (string, error)
}

func (d *Daemon) callers() Callers {
	if d.Callers != nil {
		return d.Callers
	}
	return hostCallers
}
