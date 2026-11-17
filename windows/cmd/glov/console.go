package main

import (
	"io"
	"unicode/utf8"
)

// consoleText writes a guest's serial console output to w without the
// escape sequences and control characters in it, which must not drive the
// terminal: the guest writes the console. Line feeds and tabs stay. A
// sequence or character split across writes is held back until it ends.
type consoleText struct {
	w io.Writer
	// esc is the escape sequence being skipped: 0 for none, 1 after ESC,
	// '[' in a CSI sequence, ']' in an OSC string (until BEL or ESC \).
	esc     byte
	pending []byte // an incomplete UTF-8 character
}

func (c *consoleText) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	data := append(c.pending, p...)
	c.pending = nil
	for i := 0; i < len(data); {
		b := data[i]
		switch c.esc {
		case 1:
			switch b {
			case '[', ']':
				c.esc = b
			default:
				c.esc = 0 // a two-byte sequence, such as ESC 7
			}
			i++
			continue
		case '[':
			if b >= 0x40 && b <= 0x7e {
				c.esc = 0
			}
			i++
			continue
		case ']':
			if b == 0x07 {
				c.esc = 0
			} else if b == 0x1b {
				c.esc = 1 // ESC \ ends it; the \ is skipped as a two-byte sequence
			}
			i++
			continue
		}
		if b == 0x1b {
			c.esc = 1
			i++
			continue
		}
		if b < utf8.RuneSelf {
			if b == '\n' || b == '\t' || (b >= 0x20 && b != 0x7f) {
				out = append(out, b)
			}
			i++
			continue
		}
		if !utf8.FullRune(data[i:]) {
			c.pending = append([]byte(nil), data[i:]...)
			break
		}
		r, n := utf8.DecodeRune(data[i:])
		// C1 controls (U+0080 to U+009F) include CSI and OSC too.
		if r != utf8.RuneError && (r < 0x80 || r > 0x9f) {
			out = append(out, data[i:i+n]...)
		}
		i += n
	}
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
