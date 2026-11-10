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

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func run(args []string) error {
	flags := flag.NewFlagSet("automation-gateway-zabbix", flag.ContinueOnError)
	configPath := flags.String("config", "", "strict YAML configuration file")
	check := flags.Bool("check", false, "validate configuration and protected files, then exit")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		if *configPath != "" || *check || flags.NArg() != 0 {
			return fmt.Errorf("-version must be used alone")
		}
		fmt.Printf("automation-gateway-zabbix %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}
	if *configPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("-config is required; -check is the only other supported option")
	}
	cfg, err := zabbixbridge.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *check {
		logger.Info("zabbix bridge configuration valid", "gateway_url", cfg.GatewayURL, "zabbix_api_url", cfg.ZabbixAPIURL, "zabbix_host", cfg.ZabbixHost)
		return nil
	}
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
