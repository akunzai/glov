package mirror

import "github.com/akunzai/glov/guest/proto"

func lookupOwner(name string) (*owner, error) {
	return nil, proto.Errorf(proto.CodeInvalidParams, "owner %s: sync.apply takes an owner on Linux guests only", name)
}
