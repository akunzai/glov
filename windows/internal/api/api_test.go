package api

import (
	"encoding/json/v2"
	"testing"
)

func ptr(s string) *string { return &s }

// The status JSON is a contract with scripts and agents; its keys match the
// macOS daemon's VMInfo and GuestServiceStatus, which leave out unset fields.
func TestVMInfoJSON(t *testing.T) {
	for _, tc := range []struct {
		info VMInfo
		want string
	}{
		{VMInfo{Name: "dev", State: "stopped", Engine: "hcs"}, `{"name":"dev","state":"stopped","engine":"hcs"}`},
		{
			VMInfo{Name: "dev", State: "running", Engine: "hcs", GuestService: &GuestService{
				State: "ready", OS: ptr("linux"), Version: ptr("1.2.3"), BootID: ptr("b1"),
			}},
			`{"name":"dev","state":"running","engine":"hcs","guestService":{"state":"ready","os":"linux","version":"1.2.3","bootId":"b1"}}`,
		},
	} {
		got, err := json.Marshal(tc.info)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("JSON =\n%s\nwant\n%s", got, tc.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		info VMInfo
		want string
	}{
		{VMInfo{Name: "dev", State: "stopped", Engine: "hcs"}, "dev: stopped [hcs]"},
		{VMInfo{Name: "dev", State: "running", Engine: "hcs", GuestService: &GuestService{State: "connecting"}}, "dev: running [hcs], Guest Service connecting"},
		{
			VMInfo{Name: "dev", State: "running", Engine: "hcs", GuestService: &GuestService{State: "unresponsive", Problem: ptr("silent for 14 s")}},
			"dev: running [hcs], Guest Service unresponsive (silent for 14 s)",
		},
		{
			VMInfo{Name: "dev", State: "running", Engine: "hcs", GuestService: &GuestService{State: "ready", Problem: ptr("guest network: \x1b[2Jboom"),
				Update: &GuestCheck{State: UpdateAvailable}}},
			"dev: running [hcs], Guest Service ready (guest network: [2Jboom), update available: glov vm guest update dev",
		},
	} {
		if got := tc.info.Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
	c := GuestCheck{State: UpdateUnknown, CurrentVersion: ptr("v1\x07"), Problem: ptr("bad\r\nline")}
	if got, want := c.Describe(), "Guest Service v1 -> unknown: unknown (badline)"; got != want {
		t.Errorf("GuestCheck.Describe() = %q, want %q", got, want)
	}
}
