package proto

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
)

// Sender is implemented by a handler result that wants to run code once its
// response is on the wire (exec.start uses it so that output notifications
// never overtake the reply carrying the pid; power.reboot uses it to act after
// replying). Sent is called once per reply: delivered is false when the reply
// could not be written, so the host never learned of the result and the
// Sender must undo or skip what it would have done.
type Sender interface{ Sent(delivered bool) }

type handler struct {
	fn    func(params jsontext.Value) (any, error)
	async bool
}

// Server dispatches requests to registered methods.
type Server struct{ methods map[string]handler }

// NewServer returns a Server with no methods.
func NewServer() *Server { return &Server{methods: map[string]handler{}} }

func register[P, R any](s *Server, name string, async bool, fn func(P) (R, error)) {
	s.methods[name] = handler{async: async, fn: func(raw jsontext.Value) (any, error) {
		var p P
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, Errorf(CodeInvalidParams, "invalid params: %v", err)
			}
		}
		return fn(p)
	}}
}

// Handle registers a method that is served inline: the next request is not
// read until it returns, which keeps requests such as exec.stdin ordered.
func (s *Server) Handle[P, R any](name string, fn func(P) (R, error)) {
	register(s, name, false, fn)
}

// HandleAsync registers a method served in its own goroutine, for operations
// that may block for long (file access on a slow or network path).
func (s *Server) HandleAsync[P, R any](name string, fn func(P) (R, error)) {
	register(s, name, true, fn)
}

type request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Method  string         `json:"method"`
	Params  jsontext.Value `json:"params"`
}

// Serve reads and dispatches frames until the stream fails; it returns that
// error. A ctx cancellation closes the connection.
func (s *Server) Serve(ctx context.Context, c *Conn) error {
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	for {
		body, err := ReadFrame(c.rwc)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		s.dispatch(c, body)
	}
}

func (s *Server) dispatch(c *Conn, body []byte) {
	null := jsontext.Value("null")
	var req request
	// reply logs a reply that could not be sent: the host never sees it, and
	// without this line nothing would tell why its request went unanswered.
	reply := func(id jsontext.Value, res any, perr *Error) bool {
		if err := c.respond(id, res, perr); err != nil {
			slog.Warn("reply not sent", "method", req.Method, "id", string(id), "err", err)
			return false
		}
		return true
	}
	if err := json.Unmarshal(body, &req); err != nil {
		if _, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
			reply(null, nil, Errorf(CodeParse, "parse error"))
		} else {
			reply(null, nil, Errorf(CodeInvalidRequest, "invalid request"))
		}
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		reply(null, nil, Errorf(CodeInvalidRequest, "invalid request"))
		return
	}
	notif := len(req.ID) == 0
	h, ok := s.methods[req.Method]
	if !ok {
		if !notif {
			reply(req.ID, nil, Errorf(CodeNoMethod, "method not found: %s", req.Method))
		}
		return
	}
	run := func() {
		res, err := h.fn(req.Params)
		if notif {
			return
		}
		if err != nil {
			slog.Debug("request failed", "method", req.Method, "err", err)
			reply(req.ID, nil, FromOS(err))
			return
		}
		delivered := reply(req.ID, res, nil)
		if s, ok := res.(Sender); ok {
			s.Sent(delivered)
		}
	}
	if h.async {
		go run()
	} else {
		run()
	}
}
