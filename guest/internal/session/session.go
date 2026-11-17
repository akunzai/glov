// Package session finds the user signed in on a Linux guest from systemd-logind's
// session records. The parsing is portable so it can be tested anywhere; only
// Active reads the real /run/systemd/sessions.
package session

import (
	"bufio"
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// MinUID is the lowest uid counted as a person; system accounts and root do
// not count as "the signed-in user".
const MinUID = 1000

// Session is the subset of a logind session record the Guest Service uses.
type Session struct {
	ID      string
	UID     int
	User    string
	Type    string // x11, wayland, mir, tty, unspecified
	Class   string // user, greeter, ...
	State   string // online, active, closing
	Display string // X11 DISPLAY, when logind recorded one
	Remote  bool   // ssh and the like
	Seat    string
}

// Graphical reports whether the session runs a display server.
func (s Session) Graphical() bool {
	return s.Type == "x11" || s.Type == "wayland" || s.Type == "mir"
}

// rank orders candidates: a graphical session first, then a local seat or tty
// session, then a remote one; an active session beats an online one.
func (s Session) rank() int {
	r := 0
	switch {
	case s.Graphical():
		r = 4
	case !s.Remote:
		r = 2
	}
	if s.State == "active" {
		r++
	}
	return r
}

// Parse reads one /run/systemd/sessions/<id> record (KEY=VALUE lines).
func Parse(id, content string) (Session, error) {
	s := Session{ID: id, UID: -1}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "UID":
			n, err := strconv.Atoi(v)
			if err != nil {
				return s, err
			}
			s.UID = n
		case "USER":
			s.User = v
		case "TYPE":
			s.Type = v
		case "CLASS":
			s.Class = v
		case "STATE":
			s.State = v
		case "DISPLAY":
			s.Display = v
		case "REMOTE":
			s.Remote = v == "1"
		case "SEAT":
			s.Seat = v
		}
	}
	if s.UID < 0 {
		return s, errors.New("session record has no UID")
	}
	return s, nil
}

// Pick chooses the signed-in user's session from candidates, or false when
// nobody qualifies. Only user-class sessions of a person (uid >= MinUID) that
// are not closing count. Ties go to the oldest session (lowest numeric id).
func Pick(all []Session) (Session, bool) {
	var ok []Session
	for _, s := range all {
		if s.Class == "user" && s.UID >= MinUID && s.State != "closing" && s.User != "" {
			ok = append(ok, s)
		}
	}
	if len(ok) == 0 {
		return Session{}, false
	}
	return slices.MinFunc(ok, func(a, b Session) int {
		return cmp.Or(cmp.Compare(b.rank(), a.rank()), cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
	}), true
}

// Active reads the session records under dir (normally /run/systemd/sessions)
// and picks the signed-in session. Only regular files are session records:
// the per-seat "seat0" symlinks are skipped, and so are the "<id>.ref" FIFOs
// logind keeps beside each record, which a read would block on forever.
func Active(dir string) (Session, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Session{}, false
	}
	var all []Session
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if s, err := Parse(e.Name(), string(b)); err == nil {
			all = append(all, s)
		}
	}
	return Pick(all)
}
