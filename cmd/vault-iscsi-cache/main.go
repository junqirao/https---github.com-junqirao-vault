// Command vault-iscsi-cache runs the standalone iSCSI read-cache proxy.
//
// Usage:
//
//	vault-iscsi-cache -config cache.yaml
//	vault-iscsi-cache -config cache.yaml -check
//
// The command is self contained; it is not wired into the vault server or
// agent. Host applications embed the module instead by importing
// vault/internal/iscsicache.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"

	"vault/internal/iscsicache"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vault-iscsi-cache: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "iscsi-cache.yaml", "path to the YAML configuration")
		checkOnly  = flag.Bool("check", false, "load the configuration, dial the backend and exit")
		logLevel   = flag.String("log-level", "info", "log level: debug, info, warn, error")
		logFile    = flag.String("log-file", "", "write logs to this file instead of stderr")
	)
	flag.Parse()

	logger, closeLog, err := newLogger(*logLevel, *logFile)
	if err != nil {
		return err
	}
	defer closeLog()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg iscsicache.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", *configPath, err)
	}
	cfg.Logger = logger

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := iscsicache.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer svc.Close()

	dev := svc.DeviceInfo()
	logger.Info("proxy ready",
		"listen", svc.Addr().String(),
		"target_iqn", cfg.TargetIQN,
		"backend", cfg.Backend.Address,
		"vendor", dev.Vendor, "product", dev.Product,
		"block_size", dev.BlockSize, "block_count", dev.BlockCount,
		"capacity_bytes", dev.CapacityBytes())
	if *checkOnly {
		return nil
	}

	err = svc.Start(ctx)
	st := svc.Stats()
	logger.Info("proxy stopped",
		"reads", st.Proxy.Reads, "writes", st.Proxy.Writes,
		"l1_hits", st.Cache.L1Hits, "partial_hits", st.Cache.PartialHits,
		"l2_hits", st.Cache.L2Hits, "backend_reads", st.Cache.BackendReads)
	return err
}

func newLogger(level, path string) (*slog.Logger, func(), error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}
	w := os.Stderr
	closeFn := func() {}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		w = f
		closeFn = func() { _ = f.Close() }
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), closeFn, nil
}
