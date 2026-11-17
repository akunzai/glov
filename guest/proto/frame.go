// Package proto implements the Guest Service wire protocol: length-prefixed
// JSON-RPC 2.0 frames and a request dispatcher. See docs/guest-protocol.md.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrame is the largest accepted or sent frame body.
const MaxFrame = 1 << 20

// ErrFrameTooLarge reports a frame body over MaxFrame.
var ErrFrameTooLarge = errors.New("proto: frame larger than 1 MiB")

// ErrFrameCut reports a write that failed after part of the frame went out:
// the peer now reads the next frame's bytes as this one's body. Closing the
// port would not help: virtio-serial gives the host no EOF.
var ErrFrameCut = errors.New("proto: frame cut short")

// ReadFrame reads one frame body. A length over MaxFrame is ErrFrameTooLarge
// and leaves the stream unsynchronised: the caller must drop the connection.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, ErrFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// WriteFrame writes header and body in a single Write so that a transport
// with message semantics never sees a split frame.
func WriteFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	if n, err := w.Write(buf); err != nil {
		if n > 0 {
			return fmt.Errorf("%w after %d of %d bytes: %w", ErrFrameCut, n, len(buf), err)
		}
		return err
	}
	return nil
}
