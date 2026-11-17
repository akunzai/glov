package main

import (
	"context"
	"fmt"
	"os"

	"github.com/akunzai/glov/windows/internal/api"
)

// maxTrustFile bounds the PEM file glov vm trust add reads.
const maxTrustFile = 1 << 20

// runTrust runs glov vm trust list, add and remove.
func runTrust(ctx context.Context, c caller, cmd command) error {
	var r api.Trust
	var err error
	switch cmd.name {
	case "trust list":
		err = call(ctx, c, api.MethodTrustList, api.TrustParams{VM: cmd.vm}, &r)
	case "trust add":
		info, serr := os.Stat(cmd.file)
		if serr != nil {
			return serr
		}
		if info.Size() > maxTrustFile {
			return fmt.Errorf("%s is over %d KiB: not one PEM certificate", cmd.file, maxTrustFile>>10)
		}
		pem, rerr := os.ReadFile(cmd.file)
		if rerr != nil {
			return rerr
		}
		err = call(ctx, c, api.MethodTrustAdd, api.TrustParams{VM: cmd.vm, Name: cmd.names[0], Certificate: string(pem)}, &r)
	case "trust remove":
		err = call(ctx, c, api.MethodTrustRemove, api.TrustParams{VM: cmd.vm, Name: cmd.names[0]}, &r)
	}
	if err != nil {
		return err
	}
	fmt.Print(describeTrust(r))
	return nil
}
