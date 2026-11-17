package egress

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var policy = NewPolicy([]string{"api.example.com", "*.ubuntu.com", "registry.example.org:8443", "203.0.113.7", "[2001:db8::7]:8080"})

func expect(t *testing.T, p Policy, host string, port int, want bool) {
	t.Helper()
	if got := p.Allows(host, port); got != want {
		t.Errorf("Allows(%q, %d) = %v, want %v", host, port, got, want)
	}
}

func TestAnExactHostIsAllowedOnHTTPSAndHTTPOnly(t *testing.T) {
	expect(t, policy, "api.example.com", 443, true)
	expect(t, policy, "API.Example.COM.", 80, true)
	expect(t, policy, "api.example.com", 22, false)
	expect(t, policy, "example.com", 443, false)
	expect(t, policy, "evil-api.example.com", 443, false)
	expect(t, policy, "api.example.com.evil.net", 443, false)
}

func TestAWildcardMatchesNamesBelowItsSuffixButNotTheSuffix(t *testing.T) {
	expect(t, policy, "archive.ubuntu.com", 443, true)
	expect(t, policy, "a.b.ubuntu.com", 80, true)
	expect(t, policy, "ubuntu.com", 443, false)
	expect(t, policy, "notubuntu.com", 443, false)
}

func TestAnEntryWithAPortAllowsThatPortOnly(t *testing.T) {
	expect(t, policy, "registry.example.org", 8443, true)
	expect(t, policy, "registry.example.org", 443, false)
	expect(t, policy, "2001:db8::7", 8080, true)
	expect(t, policy, "2001:db8::7", 443, false)
}

func TestAnIPLiteralIsAllowedOnlyByAnIdenticalEntry(t *testing.T) {
	expect(t, policy, "203.0.113.7", 443, true)
	expect(t, policy, "203.0.113.8", 443, false)
	expect(t, NewPolicy([]string{"*.0.113.7"}), "203.0.113.7", 443, false)
}

func TestNothingIsAllowedByDefault(t *testing.T) {
	expect(t, NewPolicy(nil), "example.com", 443, false)
	expect(t, policy, "", 443, false)
	expect(t, policy, "api.example.com", 0, false)
}

func TestTheSharedReachabilityCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "egress", "reachability.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Public, Private, Never []string
		Local                  []struct {
			Address string
			Local   []string
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, private := range []bool{false, true} {
		check := func(list []string, want bool) {
			for _, a := range list {
				if got := Reachable(netip.MustParseAddr(a), nil, private); got != want {
					t.Errorf("%s (private %v): reachable %v", a, private, got)
				}
			}
		}
		check(cases.Public, true)
		check(cases.Private, private)
		check(cases.Never, false)
		for _, c := range cases.Local {
			var local []netip.Addr
			for _, l := range c.Local {
				local = append(local, netip.MustParseAddr(l))
			}
			if Reachable(netip.MustParseAddr(c.Address), local, private) {
				t.Errorf("%s with local %q (private %v): reachable", c.Address, c.Local, private)
			}
		}
	}
	if Reachable(netip.MustParseAddr("2606:2800::1"), []netip.Addr{netip.MustParseAddr("2606:2800:0::1%5")}, false) {
		t.Error("the host's own IPv6 address (with a zone) is reachable")
	}
}

func TestAnOpenVMPermitsAnyHostButReachesPrivateNetworksOnlyByAPrivateEntry(t *testing.T) {
	s := Settings{Allow: []string{"api.example.com", "gitlab.corp.example"}, PrivateAllow: []string{"gitlab.corp.example"}}
	if s.Permits("other.example.com", 443) || !s.Permits("api.example.com", 443) {
		t.Error("restricted: the allowlist does not decide")
	}
	s.Open = true
	for _, h := range []string{"other.example.com", "Other.Example.com.", "93.184.215.14", "2001:db8::7"} {
		if !s.Permits(h, 22) {
			t.Errorf("open: %s not permitted", h)
		}
	}
	for _, bad := range []string{"", "a b.com", "*.example.com"} {
		if s.Permits(bad, 443) {
			t.Errorf("open: %q permitted", bad)
		}
	}
	if s.Permits("example.com", 0) || s.Permits("example.com", 65536) {
		t.Error("open: an invalid port permitted")
	}
	if !s.ReachesPrivate("gitlab.corp.example", 443) || s.ReachesPrivate("api.example.com", 443) || s.ReachesPrivate("other.example.com", 443) {
		t.Error("open: private reach is not by the --private entries")
	}
}

func TestPrivateEntriesAreExactAndMarkedOnce(t *testing.T) {
	marked, err := MarkingPrivate(nil, []string{"GitLab.corp.example", "10.1.2.3:8443"})
	if err != nil || !slices.Equal(marked, []string{"gitlab.corp.example", "10.1.2.3:8443"}) {
		t.Fatalf("MarkingPrivate = %q, %v", marked, err)
	}
	if again, err := MarkingPrivate(marked, []string{"gitlab.corp.example."}); err != nil || !slices.Equal(again, marked) {
		t.Errorf("marking again = %q, %v", again, err)
	}
	s := Settings{Allow: []string{"api.example.com", "gitlab.corp.example", "10.1.2.3:8443"}, PrivateAllow: marked}
	if !s.ReachesPrivate("gitlab.corp.example", 443) || !s.ReachesPrivate("10.1.2.3", 8443) || s.ReachesPrivate("10.1.2.3", 443) || s.ReachesPrivate("api.example.com", 443) {
		t.Error("ReachesPrivate does not follow the marked entries")
	}
	got, err := MarkingPrivate(marked, []string{"ok.example.com", "*.corp.example"})
	if !errors.Is(err, ErrPrivateSuffix) || got != nil || err.Error() != "--private takes an exact hostname or IP address, not *.corp.example" {
		t.Errorf("a suffix: %q, %v", got, err)
	}
	if _, err := MarkingPrivate(nil, []string{"a b.com"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an invalid entry: %v", err)
	}
	if left := Unmarking(marked, []string{"GITLAB.corp.example", "other.example.com"}); !slices.Equal(left, []string{"10.1.2.3:8443"}) {
		t.Errorf("Unmarking = %q", left)
	}
	if left := Unmarking(marked, []string{"gitlab.corp.example", "10.1.2.3:8443"}); left != nil {
		t.Errorf("Unmarking all = %q", left)
	}
}

func TestEntriesAreStoredInCanonicalFormOnceEach(t *testing.T) {
	got, err := Allowing([]string{"api.example.com"},
		[]string{"API.Example.com.", "*.Ubuntu.COM", "registry.example.org:8443", "2001:0db8::0007", "[2001:db8::7]:8080", "api.example.com:443"})
	want := []string{"api.example.com", "*.ubuntu.com", "registry.example.org:8443", "2001:db8::7", "[2001:db8::7]:8080", "api.example.com:443"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("Allowing = %q, %v; want %q", got, err, want)
	}
}

func TestAnInvalidEntryChangesNothingAndIsNamed(t *testing.T) {
	for _, bad := range []string{"", "*", "*.", "a*.example.com", "example.com:0", "example.com:99999", "example.com:http", "[::1", "a b.com", "https://example.com", "fe80::1%eth0"} {
		got, err := Allowing([]string{"api.example.com"}, []string{"ok.example.com", bad})
		if !errors.Is(err, ErrInvalid) || got != nil || !strings.Contains(err.Error(), `"`+bad+`"`) || !strings.Contains(err.Error(), "*.suffix") {
			t.Errorf("Allowing(%q) = %q, %v", bad, got, err)
		}
	}
}

func TestRemovingMatchesCanonicalFormsAndNamesWhatIsMissing(t *testing.T) {
	allow := []string{"api.example.com", "*.ubuntu.com", "2001:db8::7"}
	got, err := Removing(allow, []string{"API.example.com.", "2001:0db8:0::7"})
	if err != nil || !slices.Equal(got, []string{"*.ubuntu.com"}) {
		t.Errorf("Removing = %q, %v", got, err)
	}
	_, err = Removing(allow, []string{"*.ubuntu.com", "other.example.com"})
	if !errors.Is(err, ErrNotListed) || err.Error() != "not in the allowlist: other.example.com" {
		t.Errorf("Removing a missing entry: %v", err)
	}
}
