package egress

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Decisions.
const (
	Allow = "allow"
	// Deny is refused by the policy.
	Deny = "deny"
	// Fail is allowed, but unreachable.
	Fail = "fail"
)

// Record is one connection through the proxy: metadata only, never a body
// or a header value. Its JSON is the macOS daemon's.
type Record struct {
	// Time is an RFC 3339 UTC time, to the second.
	Time     string `json:"time"`
	VM       string `json:"vm"`
	Decision string `json:"decision"`
	// Host and Port are nil for a request too malformed to name one.
	Host *string `json:"host,omitempty"`
	Port *int    `json:"port,omitempty"`
	// BytesIn and BytesOut are the bytes to the guest and from it.
	BytesIn  int64  `json:"in"`
	BytesOut int64  `json:"out"`
	Reason   string `json:"reason,omitempty"`
	// Method, Path (without the query) and Status describe a request the
	// proxy read inside TLS; Secrets names the secrets it put in, never a
	// value.
	Method  string   `json:"method,omitempty"`
	Path    string   `json:"path,omitempty"`
	Status  int      `json:"status,omitzero"`
	Secrets []string `json:"secrets,omitempty"`
}

// NewRecord is a record made now; host "" names no target.
func NewRecord(vm, decision, host string, port int, reason string) Record {
	r := Record{Time: time.Now().UTC().Format(time.RFC3339), VM: vm, Decision: decision, Reason: reason}
	if host != "" {
		r.Host, r.Port = &host, &port
	}
	return r
}

// Target is host:port, [v6]:port, or "-".
func (r Record) Target() string {
	if r.Host == nil || r.Port == nil {
		return "-"
	}
	return target(*r.Host, *r.Port)
}

func target(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// Summary is "<decision> <target>[ <method> <path>][ <status>]
// in=<bytes> out=<bytes>[ secrets=<a,b>][ (<reason>)]".
func (r Record) Summary() string {
	s := r.Decision + " " + r.Target()
	if r.Method != "" && r.Path != "" {
		s += " " + r.Method + " " + r.Path
	}
	if r.Status != 0 {
		s += " " + strconv.Itoa(r.Status)
	}
	s += fmt.Sprintf(" in=%d out=%d", r.BytesIn, r.BytesOut)
	if len(r.Secrets) > 0 {
		s += " secrets=" + strings.Join(r.Secrets, ",")
	}
	if r.Reason != "" {
		s += " (" + r.Reason + ")"
	}
	return s
}

// LogFile is the records' file in the VM's directory; past LogCap it is
// renamed to LogFile+".1", replacing that.
const (
	LogFile = "egress.log"
	LogCap  = 1 << 20
)

// logMu serialises every append and read: on Windows a file that is open
// cannot be renamed, so a read must not overlap a rotation.
var logMu sync.Mutex

// Append adds r to the log at path, rotating it past limit bytes. A failure
// to write is dropped: glovd's log has the same line.
func Append(path string, limit int64, r Record) {
	data, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return
	}
	data = append(data, '\n')
	logMu.Lock()
	defer logMu.Unlock()
	if st, err := os.Stat(path); err == nil && st.Size() >= limit {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
}

// Page is what Read returns; pass Cursor back to read only what comes after.
type Page struct {
	Records []Record `json:"records"`
	Cursor  string   `json:"cursor"`
}

// Read returns the records after cursor (all of them), or without one the
// last limit; denied keeps deny and fail. The cursor names the file (by
// its file ID) and offset read up to, so it survives one rotation.
func Read(path, cursor string, denied bool, limit int) Page {
	logMu.Lock()
	defer logMu.Unlock()
	var records []Record
	var current chunk
	if id, offset, ok := parseCursor(cursor); ok {
		if same := readLines(path, offset, &id); same.id == id {
			current = same
		} else {
			// Rotated since: finish the file the cursor was in, then read
			// the new one from its start.
			records = readLines(path+".1", offset, &id).records
			current = readLines(path, 0, nil)
		}
	} else {
		cursor = ""
		records = readLines(path+".1", 0, nil).records
		current = readLines(path, 0, nil)
	}
	records = append(records, current.records...)
	if denied {
		kept := records[:0]
		for _, r := range records {
			if r.Decision != Allow {
				kept = append(kept, r)
			}
		}
		records = kept
	}
	if cursor == "" && len(records) > limit {
		records = records[len(records)-limit:]
	}
	if records == nil {
		records = []Record{}
	}
	return Page{Records: records, Cursor: fmt.Sprintf("%d:%d", current.id, current.end)}
}

type chunk struct {
	records []Record
	// id is of the file actually read (0: none), so a rotation during the
	// read cannot pair it with another file's offset.
	id  uint64
	end int64
}

func parseCursor(s string) (uint64, int64, bool) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, false
	}
	id, err1 := strconv.ParseUint(a, 10, 64)
	offset, err2 := strconv.ParseInt(b, 10, 64)
	return id, offset, err1 == nil && err2 == nil && offset >= 0
}

// readLines reads the complete lines of path from offset on, and the offset
// after the last one (a line still being written waits). With ifID, only
// when the file is that one (else just its ID, offset 0, nothing read).
func readLines(path string, offset int64, ifID *uint64) chunk {
	f, err := os.Open(path)
	if err != nil {
		return chunk{}
	}
	defer f.Close()
	id, err := fileID(f)
	if err != nil {
		return chunk{}
	}
	st, err := f.Stat()
	if err != nil {
		return chunk{}
	}
	c := chunk{id: id, end: offset}
	if ifID != nil && *ifID != id || st.Size() < offset {
		c.end = 0
		return c
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return c
	}
	data, err := io.ReadAll(f)
	last := bytes.LastIndexByte(data, '\n')
	if err != nil || last < 0 {
		return c
	}
	for line := range bytes.SplitSeq(data[:last], []byte{'\n'}) {
		var r Record
		if json.Unmarshal(line, &r) == nil {
			c.records = append(c.records, r)
		}
	}
	c.end = offset + int64(last+1)
	return c
}
