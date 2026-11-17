package guestservice

import (
	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

// registerLive serves a live share's calls: the watch and the reads that a
// change notification leads to, beside the request loop (a watch lists the
// tree, a read may block on a slow file).
func registerLive(s *proto.Server, mirrors *mirror.Manager, notify func(string, any)) {
	mirrors.SetNotify(notify)
	s.HandleAsync("sync.watch", mirrors.Watch)
	s.HandleAsync("sync.read", mirrors.Read)
	s.Handle("sync.unwatch", mirrors.Unwatch)
}
