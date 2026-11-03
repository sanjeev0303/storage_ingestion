// Command osp-agent watches folders on an instrument, robot or workstation
// and sends what appears to the platform (docs/edge.md).
//
//	osp-agent -config agent.json          run until stopped
//	osp-agent -config agent.json -once    one pass, then exit
//	osp-agent -config agent.json -status  what is waiting, then exit
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sanjeev0303/object-storage-platform/sdk/go/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "osp-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	config := flag.String("config", "agent.json", "the configuration file")
	once := flag.Bool("once", false, "do one pass and exit")
	status := flag.Bool("status", false, "print what is waiting and exit")
	flag.Parse()

	cfg, err := agent.LoadConfig(*config)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()
	if *status {
		return json.NewEncoder(os.Stdout).Encode(a.Status())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		if err := a.Tick(ctx); err != nil {
			return err
		}
		return a.Heartbeat(ctx)
	}
	log.Info("osp-agent started", "version", agent.Version, "watches", len(cfg.Watches))
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
