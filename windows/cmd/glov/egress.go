package main

import (
	"slices"
	"strconv"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
)

// describeAllowlist is an allowlist in one line, as macOS glov has it.
func describeAllowlist(e api.Egress) string {
	if e.Open {
		if len(e.Allow) == 0 {
			return describeEgressMode(e)
		}
		return describeEgressMode(e) + "\nallowlist: " + strings.Join(markedEntries(e), ", ")
	}
	if len(e.Allow) == 0 {
		return e.VM + ": nothing allowed (every connection is denied)"
	}
	return e.VM + " allows: " + strings.Join(markedEntries(e), ", ")
}

// allowlistLines is glov egress list's output: the mode when open, then
// one entry per line.
func allowlistLines(e api.Egress) string {
	switch {
	case e.Open && len(e.Allow) == 0:
		return describeEgressMode(e)
	case e.Open:
		return describeEgressMode(e) + "\n" + strings.Join(markedEntries(e), "\n")
	case len(e.Allow) == 0:
		return describeAllowlist(e)
	}
	return strings.Join(markedEntries(e), "\n")
}

// describeEgressMode is the VM's egress mode in one line.
func describeEgressMode(e api.Egress) string {
	if e.Open {
		return e.VM + ": egress open: any public host, through the egress proxy; the allowlist applies again after glov vm configure " + e.VM + " --egress restricted"
	}
	return e.VM + ": egress restricted: only the allowlist"
}

// markedEntries are the entries, each allowed with --private followed by
// " (private)".
func markedEntries(e api.Egress) []string {
	out := make([]string, len(e.Allow))
	for i, a := range e.Allow {
		out[i] = a
		if slices.Contains(e.Private, a) {
			out[i] += " (private)"
		}
	}
	return out
}

// describeEgress is "<UTC time> <decision> <target> in=<bytes>
// out=<bytes>[ (<reason>)]".
func describeEgress(r egress.Record) string { return r.Time + " " + r.Summary() }

// describeMCPEdits says who may change e's allowlist.
func describeMCPEdits(e api.Egress) string {
	if e.MCPEdits {
		return e.VM + ": MCP clients may change the egress allowlist"
	}
	return e.VM + ": only glov may change the egress allowlist"
}

// describeEgressLine is vm status's egress line of an isolated VM.
func describeEgressLine(e api.Egress) string {
	if e.Open {
		return "  egress: open (any public host, through the proxy)"
	}
	return "  egress: restricted (" + strconv.Itoa(len(e.Allow)) + " allowed)"
}
