package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/secret"
)

// runSecret runs glov secret. No answer holds a value, and glov prints
// none; a source secret's command runs here, as the user, never in glovd.
func runSecret(ctx context.Context, c caller, cmd command) error {
	switch cmd.name {
	case "secret set":
		value, err := readValue(os.Stdin, "Value for "+cmd.names[0]+": ")
		if err != nil {
			return err
		}
		var r api.Secret
		if err := call(ctx, c, api.MethodSecretSet, api.SecretParams{Name: cmd.names[0], Hosts: cmd.hosts, Value: value, Forge: cmd.forge}, &r); err != nil {
			return err
		}
		fmt.Printf("stored secret %s for %s (%s)\n", r.Name, strings.Join(r.Hosts, ", "), secret.Describe(r.Length, r.Ending))
	case "secret source":
		argv, err := resolveCommand(cmd.argv)
		if err != nil {
			return err
		}
		value, err := runSource(ctx, cmd.names[0], argv)
		if err != nil {
			return err
		}
		var r api.Secret
		if err := call(ctx, c, api.MethodSecretSource, api.SecretParams{Name: cmd.names[0], Hosts: cmd.hosts, Value: value, Command: argv, Forge: cmd.forge}, &r); err != nil {
			return err
		}
		fmt.Printf("secret %s comes from %s for %s\n", r.Name, argv[0], strings.Join(r.Hosts, ", "))
	case "secret refresh":
		list, err := secretList(ctx, c)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(s api.Secret) bool { return s.Name == cmd.names[0] })
		if i < 0 {
			return fmt.Errorf("no secret named %s", cmd.names[0])
		}
		if err := refresh(ctx, c, list[i]); err != nil {
			return err
		}
		fmt.Printf("refreshed secret %s from %s\n", list[i].Name, list[i].Command[0])
	case "secret list":
		list, err := secretList(ctx, c)
		if err != nil {
			return err
		}
		if cmd.json {
			out, err := json.Marshal(api.Secrets{Secrets: list}, jsontext.WithIndent("  "), json.Deterministic(true))
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		}
		if len(list) == 0 {
			fmt.Println("no secrets defined")
		}
		for _, s := range list {
			fmt.Println(describeSecret(s))
		}
	case "secret remove":
		if err := call(ctx, c, api.MethodSecretRemove, api.SecretParams{Name: cmd.names[0]}, nil); err != nil {
			return err
		}
		fmt.Println("removed secret " + cmd.names[0])
	case "secret grant":
		// A source secret's value is as fresh as its command's last run.
		list, err := secretList(ctx, c)
		if err != nil {
			return err
		}
		for _, s := range commandSourcesIn(list, cmd.names) {
			if err := refresh(ctx, c, s); err != nil {
				return err
			}
		}
		var r api.SecretGrants
		err = confirmingPlainSecrets(cmd.allowPlain, isConsole(os.Stdin), askPlainSecrets, func(allow bool) error {
			return c.Call(ctx, api.MethodSecretGrant, api.SecretGrantParams{VM: cmd.vm, Names: cmd.names, AllowPlainSecrets: allow}, &r)
		})
		if err != nil {
			return err
		}
		fmt.Print(describeGrants(r))
		return nil
	case "secret revoke":
		return printGrants(ctx, c, api.MethodSecretRevoke, api.SecretGrantParams{VM: cmd.vm, Names: cmd.names})
	case "secret grants":
		return printGrants(ctx, c, api.MethodSecretGrants, api.SecretGrantParams{VM: cmd.vm})
	case "secret repos":
		p := api.SecretReposParams{VM: cmd.vm}
		switch cmd.repoAction {
		case "add":
			p.Add = cmd.repos
		case "remove":
			p.Remove = cmd.repos
		}
		var r api.SecretRepos
		if err := call(ctx, c, api.MethodSecretRepos, p, &r); err != nil {
			return err
		}
		if len(r.Repos) == 0 {
			fmt.Println(r.VM + ": no repositories")
		}
		for _, repo := range r.Repos {
			fmt.Println(repo)
		}
	}
	return nil
}

func secretList(ctx context.Context, c caller) ([]api.Secret, error) {
	var r api.Secrets
	err := call(ctx, c, api.MethodSecretList, struct{}{}, &r)
	return r.Secrets, err
}

// describeSecret is a secret's line in glov secret list, as macOS glov has
// it: name, source and hosts, and a stored value's length and ending.
func describeSecret(s api.Secret) string {
	forge := ""
	if s.Forge != "" {
		forge = " (" + s.Forge + " sign-in)"
	}
	if len(s.Command) > 0 {
		return s.Name + "\tcommand: " + strings.Join(s.Command, " ") + forge + "\t" + strings.Join(s.Hosts, ", ")
	}
	return s.Name + "\t" + s.Source + forge + "\t" + strings.Join(s.Hosts, ", ") + "\t" + secret.Describe(s.Length, s.Ending)
}

func printGrants(ctx context.Context, c caller, method string, p api.SecretGrantParams) error {
	var r api.SecretGrants
	if err := call(ctx, c, method, p, &r); err != nil {
		return err
	}
	fmt.Print(describeGrants(r))
	return nil
}

func describeGrants(r api.SecretGrants) string {
	if len(r.Names) == 0 {
		return r.VM + ": no secrets granted\n"
	}
	var b strings.Builder
	for _, n := range r.Names {
		if slices.Contains(r.Missing, n) {
			n += "\t(no longer defined)"
		}
		b.WriteString(n + "\n")
	}
	return b.String()
}

// refresh runs a source secret's command and stores what it printed.
func refresh(ctx context.Context, c caller, s api.Secret) error {
	if s.Source != secret.Command || len(s.Command) == 0 {
		return fmt.Errorf("secret %s is not taken from a command: store it again with glov secret set %s", s.Name, s.Name)
	}
	value, err := runSource(ctx, s.Name, s.Command)
	if err != nil {
		return err
	}
	return call(ctx, c, api.MethodSecretRefresh, api.SecretParams{Name: s.Name, Value: value}, nil)
}

// resolveCommand makes argv's program an absolute path, found on the
// user's PATH if it is a bare name, as it is stored and run again later.
func resolveCommand(argv []string) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, usageError("give the command after --")
	}
	program := argv[0]
	var err error
	switch {
	case filepath.IsAbs(program):
	case strings.ContainsAny(program, `\/`):
		program, err = filepath.Abs(program)
	default:
		if program, err = exec.LookPath(program); err == nil {
			program, err = filepath.Abs(program)
		} else {
			err = fmt.Errorf("%s is not on PATH", argv[0])
		}
	}
	if err != nil {
		return nil, err
	}
	return append([]string{program}, argv[1:]...), nil
}

// sourceTimeout bounds a source secret's command, as glovd's on macOS.
const sourceTimeout = 10 * time.Second

// runSource runs a source secret's command with stdin closed and stderr
// discarded, and returns its stdout trimmed. Its errors never quote the
// output.
func runSource(ctx context.Context, name string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	lim := &limited{w: &out, n: secret.MaxEntry}
	cmd.Stdout = lim
	prog := filepath.Base(argv[0])
	err := cmd.Run()
	switch {
	case lim.over:
		return "", fmt.Errorf("secret %s: %s printed over %d KiB", name, prog, secret.MaxEntry>>10)
	case ctx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("secret %s: %s did not finish within %s", name, prog, sourceTimeout)
	case err != nil:
		if e, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("secret %s: %s failed (exit %d)", name, prog, e.ExitCode())
		}
		return "", fmt.Errorf("secret %s: cannot run %s: %v", name, argv[0], err)
	}
	value := strings.TrimSpace(out.String())
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("secret %s: %s printed text that is not UTF-8", name, prog)
	}
	if value == "" {
		return "", fmt.Errorf("secret %s: %s printed nothing", name, prog)
	}
	return value, nil
}

// limited keeps the first n bytes written, and notes whether there were
// more.
type limited struct {
	w    io.Writer
	n    int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	k := min(len(p), l.n)
	l.w.Write(p[:k])
	l.n -= k
	l.over = l.over || k < len(p)
	return len(p), nil
}

// readValue reads a secret from in, trimmed: on a console typed or pasted
// with one * per character after prompt (on stderr), then its length and
// ending shown for a yes; else all of in.
func readValue(in *os.File, prompt string) (string, error) {
	h := windows.Handle(in.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		data, err := io.ReadAll(io.LimitReader(in, secret.MaxEntry+1))
		if err != nil {
			return "", fmt.Errorf("read the value from stdin: %w", err)
		}
		if len(data) > secret.MaxEntry {
			return "", fmt.Errorf("the value is over %d KiB", secret.MaxEntry>>10)
		}
		return strings.TrimSpace(string(data)), nil
	}
	value, err := readMasked(h, mode, prompt)
	if err != nil {
		return "", err
	}
	if value = strings.TrimSpace(value); value == "" {
		return "", errors.New("nothing was entered")
	}
	question := "read " + secret.Describe(len([]rune(value)), secret.Ending(value))
	if odd := secret.OddCharacters(value); odd > 0 {
		s := "s"
		if odd == 1 {
			s = ""
		}
		question += fmt.Sprintf("; it has %d whitespace or control character%s inside, which tokens never have", odd, s)
	}
	fmt.Fprint(os.Stderr, question+". Use it? [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return "", errors.New("not stored")
	}
	return value, nil
}

// readMasked reads a line from the console with echo, line editing and
// Ctrl+C handling off, putting the console back however it ends.
func readMasked(h windows.Handle, mode uint32, prompt string) (string, error) {
	raw := mode &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT)
	if err := windows.SetConsoleMode(h, raw); err != nil {
		return "", fmt.Errorf("turn off the console's echo: %w", err)
	}
	defer func() {
		windows.SetConsoleMode(h, mode)
		fmt.Fprintln(os.Stderr)
	}()
	fmt.Fprint(os.Stderr, prompt)
	var e secret.Entry
	var high uint16
	for {
		var buf [1]uint16
		var n uint32
		if err := windows.ReadConsole(h, &buf[0], 1, &n, nil); err != nil || n == 0 {
			return "", errors.New("the console closed")
		}
		u := buf[0]
		var r rune
		switch {
		case utf16.IsSurrogate(rune(u)) && u < 0xdc00:
			high = u
			continue
		case utf16.IsSurrogate(rune(u)):
			r, high = utf16.DecodeRune(rune(high), rune(u)), 0
		default:
			r = rune(u)
		}
		step, echo := e.Feed(r)
		fmt.Fprint(os.Stderr, echo)
		switch step {
		case secret.Done:
			return e.Value(), nil
		case secret.Cancelled:
			return "", errors.New("cancelled")
		case secret.TooLong:
			return "", fmt.Errorf("over %d KiB at the prompt; pipe the value on stdin instead", secret.MaxEntry>>10)
		}
	}
}
