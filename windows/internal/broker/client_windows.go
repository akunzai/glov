package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// ErrNotInstalled is returned when no broker answers.
var ErrNotInstalled = errors.New("glov-broker is not running: install it once with `glov host install-broker` in an elevated shell")

// Client is one connection to the broker. VMs started on it without
// KeepRunning are turned off when it closes.
type Client struct {
	rpc *rpc.Client
	// Broker is the broker's hello.
	Broker Hello
}

// Dial connects to the broker at impersonation level, which it needs to
// open the caller's disks as the caller, refuses a pipe LocalSystem does
// not own, and checks the broker's protocol.
func Dial(ctx context.Context) (*Client, error) {
	c, err := winio.DialPipeAccessImpLevel(ctx, PipeName, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelImpersonation)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, winio.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrNotInstalled
		}
		return nil, fmt.Errorf("connect to glov-broker: %w", err)
	}
	owner, err := winsec.PipeOwner(c)
	if err == nil && owner != winsec.SYSTEM {
		err = fmt.Errorf("%s is owned by %s, not LocalSystem: another program holds glov-broker's pipe", PipeName, owner)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	cl := &Client{rpc: rpc.New(c, nil)}
	if err := cl.rpc.Call(ctx, MethodHello, Hello{Protocol: Protocol}, &cl.Broker); err != nil {
		cl.Close()
		return nil, fmt.Errorf("glov-broker: %w", err)
	}
	if cl.Broker.Protocol < MinProtocol {
		cl.Close()
		return nil, fmt.Errorf("glov-broker %s speaks protocol %d, older than this glovd needs (%d): upgrade it with `glov host install-broker` in an elevated shell",
			cl.Broker.Version, cl.Broker.Protocol, MinProtocol)
	}
	return cl, nil
}

// call makes a request. A broker older than a feature answers that it
// has no such method, which names the fix.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	err := c.rpc.Call(ctx, method, params, result)
	if e, ok := errors.AsType[*proto.Error](err); ok && e.Code == proto.CodeNoMethod {
		return fmt.Errorf("glov-broker %s (protocol %d) has no %s, which this glovd needs: upgrade it with `glov host install-broker` in an elevated shell",
			c.Broker.Version, c.Broker.Protocol, method)
	}
	return err
}

// Close ends the connection.
func (c *Client) Close() error { return c.rpc.Close() }

// Done is closed once the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.rpc.Done() }

func (c *Client) NewID(ctx context.Context) (string, error) {
	var r IDParams
	err := c.call(ctx, MethodNewID, nil, &r)
	return r.ID, err
}

func (c *Client) ReleaseID(ctx context.Context, id string) error {
	return c.call(ctx, MethodReleaseID, IDParams{ID: id}, nil)
}

// Create creates a VM; Start starts it. Create returns ErrAlreadyRunning
// for a VM that runs already.
func (c *Client) Create(ctx context.Context, p CreateParams) error {
	err := c.call(ctx, MethodCreate, p, nil)
	if e, ok := errors.AsType[*proto.Error](err); ok && e.Code == CodeAlreadyRunning {
		return ErrAlreadyRunning
	}
	return err
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, MethodStart, IDParams{ID: id}, nil)
}

// ErrAlreadyRunning is Create's error for a VM that runs already.
var ErrAlreadyRunning = errors.New("the VM is already running")

func (c *Client) State(ctx context.Context, id string) (string, error) {
	var r State
	err := c.call(ctx, MethodState, IDParams{ID: id}, &r)
	return r.State, err
}

func (c *Client) Shutdown(ctx context.Context, id string) error {
	return c.call(ctx, MethodShutdown, IDParams{ID: id}, nil)
}

func (c *Client) Terminate(ctx context.Context, id string) error {
	return c.call(ctx, MethodTerminate, IDParams{ID: id}, nil)
}

func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var r Stats
	err := c.call(ctx, MethodStats, IDParams{ID: id}, &r)
	return r, err
}

// Conn is a connection to the broker that dials on first use and again
// once it breaks, as when the broker restarts for an upgrade. The VMs it
// starts keep running across that.
type Conn struct {
	mu sync.Mutex
	c  *Client
}

// dialTimeout bounds connecting to the broker.
const dialTimeout = 10 * time.Second

// Get returns the open connection, dialing if there is none.
func (b *Conn) Get(ctx context.Context) (*Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		select {
		case <-b.c.Done():
			b.c = nil
		default:
			return b.c, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := Dial(ctx)
	if err != nil {
		return nil, err
	}
	b.c = c
	return c, nil
}

// Do runs f on the connection, dialing again once if it had broken.
func (b *Conn) Do(ctx context.Context, f func(*Client) error) error {
	for attempt := 0; ; attempt++ {
		c, err := b.Get(ctx)
		if err != nil {
			return err
		}
		err = f(c)
		if errors.Is(err, rpc.ErrClosed) && attempt == 0 {
			continue
		}
		return err
	}
}

// The Conn's methods are the Client's, each on a working connection.

func (b *Conn) NewID(ctx context.Context) (id string, err error) {
	err = b.Do(ctx, func(c *Client) error { id, err = c.NewID(ctx); return err })
	return id, err
}

func (b *Conn) ReleaseID(ctx context.Context, id string) error {
	return b.Do(ctx, func(c *Client) error { return c.ReleaseID(ctx, id) })
}

func (b *Conn) Create(ctx context.Context, p CreateParams) error {
	return b.Do(ctx, func(c *Client) error { return c.Create(ctx, p) })
}

// Start starts a VM Create made on the same connection: a broken
// connection in between fails it.
func (b *Conn) Start(ctx context.Context, id string) error {
	c, err := b.Get(ctx)
	if err != nil {
		return err
	}
	return c.Start(ctx, id)
}

func (b *Conn) State(ctx context.Context, id string) (state string, err error) {
	err = b.Do(ctx, func(c *Client) error { state, err = c.State(ctx, id); return err })
	return state, err
}

func (b *Conn) Shutdown(ctx context.Context, id string) error {
	return b.Do(ctx, func(c *Client) error { return c.Shutdown(ctx, id) })
}

func (b *Conn) Terminate(ctx context.Context, id string) error {
	return b.Do(ctx, func(c *Client) error { return c.Terminate(ctx, id) })
}

func (b *Conn) Stats(ctx context.Context, id string) (s Stats, err error) {
	err = b.Do(ctx, func(c *Client) error { s, err = c.Stats(ctx, id); return err })
	return s, err
}

// ListenConsole creates VM id's console pipe (ConsolePipe), which only
// SYSTEM, this user and the VM's worker can open.
func ListenConsole(id string) (net.Listener, error) {
	g, err := guid.FromString(id)
	if err != nil {
		return nil, fmt.Errorf("VM ID: %w", err)
	}
	me, err := winsec.CurrentSID()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(ConsolePipe(id), &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;" + me + ")(A;;GA;;;" + hcs.VMSID(g) + ")",
	})
}
