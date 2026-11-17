//go:build unix

package mirror

import (
	"os/user"
	"strconv"

	"github.com/akunzai/glov/guest/proto"
)

// lookupOwner resolves a guest user name to its uid and primary gid.
func lookupOwner(name string) (*owner, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "owner %s: %v", name, err)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "owner %s has uid %q and gid %q", name, u.Uid, u.Gid)
	}
	return &owner{uid, gid}, nil
}
