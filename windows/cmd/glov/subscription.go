package main

import (
	"slices"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
)

// subscriptionOf is harness name's subscription, or the error naming those
// that have one.
func subscriptionOf(name string) (*harness.Subscription, error) {
	var with []string
	for _, p := range harness.All {
		if p.Subscription != nil {
			if p.Name == name {
				return p.Subscription, nil
			}
			with = append(with, p.Name)
		}
	}
	return nil, usageError("harness " + name + " has no subscription sign-in; those that do: " + strings.Join(with, ", "))
}

// shellQuote joins argv for sh, quoting each word that needs it.
func shellQuote(argv []string) string {
	words := make([]string, len(argv))
	for i, w := range argv {
		if w != "" && !strings.ContainsFunc(w, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@", r))
		}) {
			words[i] = w
		} else {
			words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// guestLoginShell is the ssh remote command that runs argv in a bash login
// shell, so it gets /etc/environment and the user's profile.
func guestLoginShell(argv []string) string {
	return "bash -lc " + shellQuote([]string{shellQuote(argv)})
}

// stillMissing are the subscription secrets r found missing that a sign-in
// in the guest did not store either.
func stillMissing(r api.SecretLogin, grants api.SecretGrants) []string {
	return slices.DeleteFunc(slices.Clone(r.Missing), func(n string) bool {
		return slices.Contains(grants.Names, n) && !slices.Contains(grants.Missing, n)
	})
}
