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

func main() {
	if err := start(); err != nil {
		fmt.Fprintf(os.Stderr, "control plane exiting: %v\n", err)
		os.Exit(1)
	}
}

func start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadProcess(os.Args[1:], os.LookupEnv)
	if err != nil {
		return err
	}
	return app.Run(ctx, cfg, os.Stderr, app.Options{Version: version})
}
