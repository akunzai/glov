//go:build !windows && !linux

package exec

import (
	"io"

	"github.com/akunzai/glov/guest/proto"
)

func startUser(StartParams) (child, io.WriteCloser, error) {
	return nil, nil, proto.Errorf(proto.CodeOS, "as: user is not supported on this OS")
}
