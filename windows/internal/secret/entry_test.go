package secret

import (
	"strings"
	"testing"
)

func feed(e *Entry, s string) (Step, string) {
	var echo strings.Builder
	for _, r := range s {
		step, out := e.Feed(r)
		echo.WriteString(out)
		if step != Echo {
			return step, echo.String()
		}
	}
	return Echo, echo.String()
}

func TestEntryEchoesAStarPerCharacterAndEdits(t *testing.T) {
	var e Entry
	step, echo := feed(&e, "ab\x08é\x1b[Dc\r")
	if step != Done || e.Value() != "aéc" || echo != "**\b \b**" {
		t.Errorf("step %v, value %q, echo %q", step, e.Value(), echo)
	}
	e = Entry{}
	if step, echo := feed(&e, "abc\x15d\n"); step != Done || e.Value() != "d" || echo != "***"+strings.Repeat("\b \b", 3)+"*" {
		t.Errorf("Ctrl+U: step %v, value %q, echo %q", step, e.Value(), echo)
	}
	for _, in := range []string{"x\x03", "\x1a"} {
		e = Entry{}
		if step, _ := feed(&e, in); step != Cancelled {
			t.Errorf("%q: step %v", in, step)
		}
	}
	e = Entry{}
	if step, _ := feed(&e, strings.Repeat("a", MaxEntry+1)); step != TooLong {
		t.Errorf("a long paste: step %v", step)
	}
}
