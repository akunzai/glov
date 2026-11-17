//go:build !linux

package secrets

import "github.com/akunzai/glov/guest/proto"

func prepare(string) error {
	return proto.Errorf(proto.CodeNoMethod, "secrets.sync is served on Linux only")
}
