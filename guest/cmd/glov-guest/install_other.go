//go:build !windows && !linux

package main

import "errors"

func install() error   { return errors.ErrUnsupported }
func uninstall() error { return errors.ErrUnsupported }
