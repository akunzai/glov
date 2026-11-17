package secret

import "strings"

// MaxEntry is the longest value accepted at the prompt, in bytes; a longer
// one is piped on stdin.
const MaxEntry = 64 << 10

// Entry is a secret typed or pasted at a console without echo (glov secret
// set): one '*' per character, so its length shows while it is entered, as
// macOS's SecretEntry.
type Entry struct {
	runes  []rune
	size   int
	escape []rune // inside an escape sequence (an arrow key, a paste marker)
}

// Step is what Feed asks for.
type Step int

const (
	// Echo writes Feed's text to the console and reads on.
	Echo Step = iota
	Done
	Cancelled
	TooLong
)

// Feed takes one character read from the console.
func (e *Entry) Feed(r rune) (Step, string) {
	if e.escape != nil {
		e.escape = append(e.escape, r)
		// ESC then one character, or ESC [ parameters ended by 0x40–0x7E.
		if len(e.escape) == 2 && r != '[' && r != 'O' || len(e.escape) > 2 && r >= 0x40 && r <= 0x7e {
			e.escape = nil
		}
		return Echo, ""
	}
	switch r {
	case 0x1b:
		e.escape = []rune{r}
		return Echo, ""
	case '\r', '\n':
		return Done, ""
	case 0x03:
		return Cancelled, ""
	case 0x04, 0x1a:
		if len(e.runes) == 0 {
			return Cancelled, ""
		}
		return Echo, ""
	case 0x7f, 0x08:
		if len(e.runes) == 0 {
			return Echo, ""
		}
		e.size -= len(string(e.runes[len(e.runes)-1]))
		e.runes = e.runes[:len(e.runes)-1]
		return Echo, "\b \b"
	case 0x15:
		n := len(e.runes)
		e.runes, e.size = nil, 0
		return Echo, strings.Repeat("\b \b", n)
	}
	if e.size+len(string(r)) > MaxEntry {
		return TooLong, ""
	}
	e.runes = append(e.runes, r)
	e.size += len(string(r))
	return Echo, "*"
}

// Value is what was entered.
func (e *Entry) Value() string { return string(e.runes) }
