package main

import "testing"

// A subscription sign-in waits on the user in a browser or at a device code,
// so the 30 s limit would report a failure after the tokens were stored.
func TestSignInCommandsHaveNoTimeLimit(t *testing.T) {
	for _, name := range []string{"secret login", "secret logout"} {
		if !longCommands[name] {
			t.Errorf("%s is not in longCommands", name)
		}
	}
}
