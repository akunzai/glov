package main

import (
	"strings"
	"testing"
)

func TestConsoleTextDropsEscapesAndControls(t *testing.T) {
	in := "\x1b[0;32m  OK  \x1b[0m Started ssh.\r\n\x1b]0;title\x07tab\there\x1b]2;t\x1b\\ é\u009b2J\x07\x08end\x1b7\n"
	want := "  OK   Started ssh.\ntab\there é2Jend\n"
	for _, size := range []int{len(in), 1, 2, 3} {
		var b strings.Builder
		c := &consoleText{w: &b}
		for i := 0; i < len(in); i += size {
			c.Write([]byte(in[i:min(i+size, len(in))]))
		}
		if b.String() != want {
			t.Errorf("chunks of %d: %q, want %q", size, b.String(), want)
		}
	}
}
