package egress

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func record(host, decision string) Record {
	r := NewRecord("box", decision, host, 443, "")
	r.Time = "2026-10-30T22:40:00Z"
	if decision == Allow {
		r.BytesIn, r.BytesOut = 10, 5
	} else {
		r.Reason = "not in the allowlist"
	}
	return r
}

func hosts(p Page) []string {
	var h []string
	for _, r := range p.Records {
		h = append(h, *r.Host)
	}
	return h
}

func TestARecordReadsAsOneMetadataLine(t *testing.T) {
	v6 := NewRecord("box", Allow, "2001:db8::7", 8080, "")
	v6.BytesIn, v6.BytesOut = 10, 5
	for _, c := range []struct {
		r    Record
		want string
	}{
		{record("api.example.com", Allow), "allow api.example.com:443 in=10 out=5"},
		{record("evil.example.net", Deny), "deny evil.example.net:443 in=0 out=0 (not in the allowlist)"},
		{record("", Deny), "deny - in=0 out=0 (not in the allowlist)"},
		{v6, "allow [2001:db8::7]:8080 in=10 out=5"},
	} {
		if got := c.r.Summary(); got != c.want {
			t.Errorf("Summary = %q, want %q", got, c.want)
		}
	}
}

func TestRecordsAreJSONLinesWithUTCTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), LogFile)
	Append(path, LogCap, record("api.example.com", Allow))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	want := `{"time":"2026-10-30T22:40:00Z","vm":"box","decision":"allow","host":"api.example.com","port":443,"in":10,"out":5}` + "\n"
	if text != want {
		t.Errorf("log = %q, want %q", text, want)
	}
}

func TestWithoutACursorTheLastRecordsComeBackAndDeniedKeepsOnlyRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), LogFile)
	for _, c := range [][2]string{{"a.com", Allow}, {"b.com", Deny}, {"c.com", Fail}, {"d.com", Allow}, {"e.com", Deny}} {
		Append(path, LogCap, record(c[0], c[1]))
	}
	if got := hosts(Read(path, "", false, 3)); !slices.Equal(got, []string{"c.com", "d.com", "e.com"}) {
		t.Errorf("last 3 = %q", got)
	}
	if got := hosts(Read(path, "", true, 10)); !slices.Equal(got, []string{"b.com", "c.com", "e.com"}) {
		t.Errorf("denied = %q", got)
	}
}

func TestACursorReturnsOnlyWhatCameAfterItAcrossARotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), LogFile)
	Append(path, 600, record("a.com", Allow))
	start := Read(path, "", false, 10)
	if got := hosts(start); !slices.Equal(got, []string{"a.com"}) {
		t.Fatalf("start = %q", got)
	}
	if got := Read(path, start.Cursor, false, 10); len(got.Records) != 0 {
		t.Errorf("nothing new: %q", hosts(got))
	}
	var want []string
	for i := range 6 {
		h := "n" + string(rune('0'+i)) + ".com"
		want = append(want, h)
		Append(path, 600, record(h, Deny))
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotation: %v", err)
	}
	next := Read(path, start.Cursor, false, 10)
	if got := hosts(next); !slices.Equal(got, want) {
		t.Errorf("after rotation = %q, want %q", got, want)
	}
	if got := Read(path, next.Cursor, false, 10); len(got.Records) != 0 {
		t.Errorf("nothing new: %q", hosts(got))
	}
}

func TestAMissingLogIsEmptyAndAPartialLastLineWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), LogFile)
	empty := Read(path, "", false, 10)
	if len(empty.Records) != 0 || empty.Records == nil {
		t.Errorf("missing log = %#v", empty.Records)
	}
	Append(path, LogCap, record("a.com", Allow))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"time":`)
	f.Close()
	r := Read(path, empty.Cursor, false, 10)
	if got := hosts(r); !slices.Equal(got, []string{"a.com"}) {
		t.Errorf("records = %q", got)
	}
	if !strings.HasSuffix(r.Cursor, ":"+strconv.Itoa(len(mustRead(t, path))-len(`{"time":`))) {
		t.Errorf("cursor %q passes the partial line", r.Cursor)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
