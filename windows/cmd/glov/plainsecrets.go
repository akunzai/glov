package main

import (
	"errors"
	"strings"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
)

// confirmingPlainSecrets sends a change with allowPlainSecrets as given;
// when glovd refuses it because a tmpfs secret's real value could then
// reach any host (api.CodePlainSecrets) and stdin is a console, it asks
// (default no) and sends it again with the confirmation, as macOS glov
// does. Without a console the refusal stands: it names the flag to pass.
// send returns glovd's error as the client gave it.
func confirmingPlainSecrets(allow, console bool, ask func(warning string) string, send func(allowPlainSecrets bool) error) error {
	err := send(allow)
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok {
		return err
	}
	if pe.Code == api.CodePlainSecrets && !allow && console {
		if a := strings.ToLower(strings.TrimSpace(ask(pe.Message))); a == "y" || a == "yes" {
			return unwrapped(send(true))
		}
	}
	return errors.New(pe.Message)
}

// unwrapped shows a glovd error by its message alone, as call does.
func unwrapped(err error) error {
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return errors.New(pe.Message)
	}
	return err
}
