package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/zabbixbridge"
)

func run(args []string) error {
	flags := flag.NewFlagSet("automation-gateway-zabbix", flag.ContinueOnError)
	configPath := flags.String("config", "", "strict YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("-config is required and is the only supported argument")
	}
	cfg, err := zabbixbridge.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon, err := zabbixbridge.New(cfg, logger)
	if err != nil {
		return err
	}
	defer daemon.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = daemon.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("zabbix bridge stopped", "error", err)
		os.Exit(1)
	}
}
