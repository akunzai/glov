package logfile

import "testing"

func TestCheckPolicy(t *testing.T) {
	const (
		users    = "S-1-5-32-545"
		authUser = "S-1-5-11"
		everyone = "S-1-1-0"
		owner    = "S-1-5-21-1-2-3-1001"
		fullCtl  = 0x1F01FF
		readExec = 0x1200A9
		modify   = 0x1301BF
	)
	protected := []ace{{sidSystem, fullCtl}, {sidAdministrator, fullCtl}}
	for _, tc := range []struct {
		name    string
		owner   string
		hasDACL bool
		aces    []ace
		ok      bool
	}{
		{"fresh protected dir (SYSTEM)", sidSystem, true, protected, true},
		{"fresh protected dir (Administrators)", sidAdministrator, true, protected, true},
		{"read-only for Users is fine", sidAdministrator, true, append(protected[:2:2], ace{users, readExec}), true},
		{"creator owner and TrustedInstaller are trusted", sidSystem, true, append(protected[:2:2], ace{sidCreatorOwner, fullCtl}, ace{"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", fullCtl}), true},
		{"planted by a user", owner, true, protected, false},
		{"Users may create files", sidAdministrator, true, append(protected[:2:2], ace{users, 0x2}), false},
		{"Authenticated Users modify", sidAdministrator, true, append(protected[:2:2], ace{authUser, modify}), false},
		{"Everyone append only", sidSystem, true, append(protected[:2:2], ace{everyone, 0x4}), false},
		{"Users may take ownership", sidSystem, true, append(protected[:2:2], ace{users, 0x80000}), false},
		{"generic all", sidSystem, true, []ace{{users, 0x10000000}}, false},
		{"no DACL", sidSystem, false, nil, false},
	} {
		if err := checkPolicy(tc.owner, tc.hasDACL, tc.aces); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
