package main

import (
	"context"
	"fmt"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
)

// runEgress runs glov egress list, allow, remove, mcp and log.
func runEgress(ctx context.Context, c caller, cmd command) error {
	if cmd.name == "egress log" {
		return printEgressLog(ctx, c, cmd)
	}
	var e api.Egress
	var err error
	switch cmd.name {
	case "egress list":
		err = call(ctx, c, api.MethodEgressList, api.EgressParams{VM: cmd.vm}, &e)
	case "egress allow":
		err = call(ctx, c, api.MethodEgressAllow, api.EgressParams{VM: cmd.vm, Hosts: cmd.hosts, Private: cmd.private}, &e)
	case "egress remove":
		err = call(ctx, c, api.MethodEgressRemove, api.EgressParams{VM: cmd.vm, Hosts: cmd.hosts}, &e)
	case "egress mcp":
		if err := call(ctx, c, api.MethodEgressMCP, api.EgressMCPParams{VM: cmd.vm, Enabled: cmd.on}, &e); err != nil {
			return err
		}
		fmt.Println(describeMCPEdits(e))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println(allowlistLines(e))
	return nil
}

// printEgressLog prints the VM's egress records, and with --follow the new
// ones every second until Ctrl+C.
func printEgressLog(ctx context.Context, c caller, cmd command) error {
	p := api.EgressLogParams{VM: cmd.vm, Denied: cmd.denied, Limit: cmd.lines}
	for {
		var page egress.Page
		if err := call(ctx, c, api.MethodEgressLog, p, &page); err != nil {
			if cmd.watch && ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, r := range page.Records {
			fmt.Println(describeEgress(r))
		}
		if !cmd.watch {
			return nil
		}
		p.Cursor, p.Limit = page.Cursor, 0
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
