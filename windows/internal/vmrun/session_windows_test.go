package vmrun

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStatus(t *testing.T) {
	s := newSession(newJobs(), nil)
	if g := s.status(); g.State != "connecting" || g.OS != nil || g.Problem != nil {
		t.Fatalf("new session = %+v", g)
	}
	user := "root"
	s.set("ready", &guestInfo{Version: "1.2", OS: "linux", User: &user, BootID: "b1"})
	if g := s.status(); g.State != "ready" || *g.Version != "1.2" || *g.OS != "linux" || *g.User != "root" || *g.BootID != "b1" {
		t.Fatalf("ready = %+v", g)
	}
	if !s.ready() {
		t.Fatal("ready() = false")
	}
	s.set("unresponsive", nil)
	s.silent = s.silent.Add(-7 * time.Second)
	g := s.status()
	if g.State != "unresponsive" || *g.Version != "1.2" || g.User != nil || g.BootID != nil || g.Problem == nil || !strings.Contains(*g.Problem, "7s") {
		t.Fatalf("unresponsive = %+v (problem %v)", g, g.Problem)
	}
	s.set("connecting", nil)
	if g := s.status(); g.Version != nil || g.Problem != nil {
		t.Fatalf("reconnecting = %+v", g)
	}
}
