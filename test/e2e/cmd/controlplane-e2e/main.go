// Command controlplane-e2e starts the shipping composition with a test-only model endpoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 {
		_, _ = fmt.Fprintln(os.Stderr,
			"control plane e2e exiting: startup accepts no service arguments; use OC_CONFIG_FILE or OC_* environment variables")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(os.LookupEnv)
	if err == nil {
		err = app.Run(ctx, cfg, os.Stderr, app.Options{
			Version: "e2e", ModelBaseURL: os.Getenv("OC_E2E_MODEL_BASE_URL"),
			InventoryInterval: time.Second,
		})
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "control plane e2e exiting: %v\n", err)
		return 1
	}
	return 0
}
