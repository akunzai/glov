package main

import (
	"bufio"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// isConsole reports whether f is a console, as glov asks only there.
func isConsole(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// askPlainSecrets prints warning on stderr and reads the answer.
func askPlainSecrets(warning string) string {
	fmt.Fprintf(os.Stderr, "warning: %s\nallow that? [y/N] ", warning)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return line
}
