//go:build unix

package secrets

import (
	"os/user"
	"strconv"
	"syscall"

	"github.com/akunzai/glov/guest/proto"
)

const noFollow = syscall.O_NOFOLLOW

func lookup(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, proto.Errorf(proto.CodeInvalidParams, "owner %s: %v", name, err)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, proto.Errorf(proto.CodeInvalidParams, "owner %s has uid %q and gid %q", name, u.Uid, u.Gid)
	}
	return uid, gid, nil
}
