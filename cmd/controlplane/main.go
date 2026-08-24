// Command controlplane starts the OpenCluster control plane.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

// version is stamped at release build time via -ldflags; "dev" otherwise.
var version = "dev"

// main is the only place that exits, so every deferred cleanup in start runs first.
func main() {
	if err := start(); err != nil {
		// The process failed before or after a logger existed; either way this write is the
		// last word. Configuration errors name the setting and never carry secret material.
		fmt.Fprintf(os.Stderr, "control plane exiting: %v\n", err)
		os.Exit(1)
	}
}

// start installs signal handling, loads configuration, and hands the process to app.
func start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadProcess(os.Args[1:], os.LookupEnv)
	if err != nil {
		return err
	}
	return app.Run(ctx, cfg, os.Stderr, app.Options{Version: version})
}
