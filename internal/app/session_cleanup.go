package app

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

func startSessionPruner(ctx context.Context, group *errgroup.Group, process assembled) {
	group.Go(func() error {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return nil
			}
			pass, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := process.database.PruneSessions(pass)
			cancel()
			if err != nil && ctx.Err() == nil {
				process.logger.ErrorContext(ctx, "session cleanup failed", slog.String("error", err.Error()))
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	})
}
