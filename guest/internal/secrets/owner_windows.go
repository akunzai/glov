package secrets

import "github.com/akunzai/glov/guest/proto"

const noFollow = 0

func lookup(name string) (int, int, error) {
	return 0, 0, proto.Errorf(proto.CodeNoMethod, "secrets.sync is served on Linux only")
}
