//go:build !windows && !linux

package guestservice_test

import (
	"testing"

	"github.com/akunzai/glov/guest/proto"
)

func TestUnsupportedPlatformActionsFailCleanly(t *testing.T) {
	h := start(t)
	h.connect()
	for _, tc := range []struct{ method, params string }{
		{"guest.stats", `{}`}, {"time.sync", `{"unixMillis":1700000000000}`},
	} {
		if r, _ := h.call(tc.method, tc.params); r.Error == nil || r.Error.Code != proto.CodeOS {
			t.Errorf("%s: error = %+v, want code 1", tc.method, r.Error)
		}
	}
	if r, _ := h.call("time.sync", `{"unixMillis":0}`); r.Error == nil || r.Error.Code != proto.CodeInvalidParams {
		t.Errorf("time.sync 0: %+v", r.Error)
	}
}
