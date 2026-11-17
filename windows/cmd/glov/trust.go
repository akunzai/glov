package main

import (
	"path/filepath"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
)

// parseTrust parses glov vm trust list|add|remove.
func parseTrust(args []string) (command, error) {
	const use = "usage: glov vm trust list <vm> | glov vm trust add <vm> <ca.pem> [--name NAME] | glov vm trust remove <vm> <name>"
	if len(args) == 0 {
		return command{}, usageError(use)
	}
	verb := map[string]string{"list": "list", "ls": "list", "add": "add", "remove": "remove", "rm": "remove"}[args[0]]
	cmd := command{name: "trust " + verb}
	var name string
	set := map[string]func(string) error{}
	if verb == "add" {
		set["name"] = func(v string) error { name = v; return nil }
	}
	pos, err := options(args[1:], set)
	if err != nil {
		return command{}, usageError(err.Error())
	}
	if len(pos) > 0 && pos[0] != "" {
		cmd.vm = pos[0]
	}
	switch {
	case cmd.vm == "":
	case verb == "list" && len(pos) == 1:
		return cmd, nil
	case verb == "add" && len(pos) == 2 && pos[1] != "":
		cmd.file = pos[1]
		if name == "" {
			base := filepath.Base(strings.ReplaceAll(pos[1], `\`, "/"))
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		cmd.names = []string{name}
		return cmd, nil
	case verb == "remove" && len(pos) == 2 && pos[1] != "":
		cmd.names = pos[1:]
		return cmd, nil
	}
	switch verb {
	case "list":
		return command{}, usageError("usage: glov vm trust list <vm>")
	case "add":
		return command{}, usageError("usage: glov vm trust add <vm> <ca.pem> [--name NAME]")
	case "remove":
		return command{}, usageError("usage: glov vm trust remove <vm> <name>")
	}
	return command{}, usageError(use)
}

// describeTrust is "<name>  <subject> until <date>  sha256 <first 16
// hex>[  (not a CA)]" per certificate, as macOS glov has it.
func describeTrust(r api.Trust) string {
	if len(r.Certificates) == 0 {
		return r.VM + ": no trusted certificates\n"
	}
	var b strings.Builder
	for _, c := range r.Certificates {
		b.WriteString(c.Name + "  " + c.Subject)
		if c.NotAfter != nil {
			b.WriteString(" until " + c.NotAfter.UTC().Format("2006-01-02"))
		}
		b.WriteString("  sha256 " + c.SHA256[:min(16, len(c.SHA256))])
		if !c.Authority {
			b.WriteString("  (not a CA)")
		}
		b.WriteString("\n")
	}
	return b.String()
}
