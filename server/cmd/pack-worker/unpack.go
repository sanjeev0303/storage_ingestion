package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// unpackOptions are the arguments of `pack-worker unpack`, which copies
// packed assets back to their own objects before packing is rolled back
// (docs/packing.md §11).
type unpackOptions struct {
	tenant, pack string
	dryRun       bool
}

// parseUnpack reads `unpack -tenant <id> (-pack <id> | -all) [-dry-run]`.
func parseUnpack(args []string) (unpackOptions, error) {
	var o unpackOptions
	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	fs.StringVar(&o.tenant, "tenant", "", "the organization whose packs are unpacked")
	fs.StringVar(&o.pack, "pack", "", "unpack only this pack")
	all := fs.Bool("all", false, "unpack every published pack of the organization")
	fs.BoolVar(&o.dryRun, "dry-run", false, "report what would be copied and change nothing")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unpack: unexpected arguments %s", strings.Join(fs.Args(), " "))
	}
	if o.tenant == "" {
		return o, errors.New("unpack needs -tenant")
	}
	if (o.pack == "") == !*all {
		return o, errors.New("unpack needs exactly one of -pack <id> and -all")
	}
	return o, nil
}
