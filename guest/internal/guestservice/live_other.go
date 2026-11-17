//go:build !linux

package guestservice

import (
	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

func registerLive(*proto.Server, *mirror.Manager, func(string, any)) {}
