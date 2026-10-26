package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zachlatta/agent-factory/factory"
)

func retirementCmd(args []string, out, diagnostics io.Writer) int {
	fs := flag.NewFlagSet("retirement-inventory", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	defaultHome, _ := os.UserHomeDir()
	home := fs.String("home", defaultHome, "home containing the local Paseo records to inventory")
	live := fs.Bool("live", false, "add read-only terminal and configured-script observations from this home's daemon")
	fs.Usage = func() {
		fmt.Fprintln(diagnostics, "usage: factory retirement-inventory [-home directory] [-live]\nRead-only JSON inventory; no archive, fetch, deletion or apply mode.\nExit 0: inventory read (refusals may remain); 1: incomplete inventory; 2: invalid arguments.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *home == "" {
		fs.Usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var r factory.RetirementInventory
	if *live {
		r = factory.InventoryRetirementLive(ctx, *home, time.Now())
	} else {
		r = factory.InventoryRetirement(ctx, *home, time.Now())
	}
	if ctx.Err() != nil {
		r.Problems = append(r.Problems, "inventory deadline exceeded")
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		fmt.Fprintln(diagnostics, "writing inventory failed")
		return 1
	}
	if len(r.Problems) > 0 {
		return 1
	}
	return 0
}
