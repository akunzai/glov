//go:build !windows

package main

import (
	"context"
	"errors"
)

// logPath is empty off Windows: the Guest Service logs to stderr.
const logPath = ""

func runsAsService() bool { return false }

func runService(func(context.Context)) error { return errors.ErrUnsupported }
