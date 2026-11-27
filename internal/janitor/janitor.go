// Package janitor runs housekeeping jobs on a timer until the server stops.
package janitor

import (
	"context"
	"time"
)

type Job func(context.Context) error

// Run calls every job each interval until ctx ends. A failing job doesn't stop the others.
func Run(ctx context.Context, every time.Duration, onError func(error), jobs ...Job) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, job := range jobs {
				if err := job(ctx); err != nil && ctx.Err() == nil {
					onError(err)
				}
			}
		}
	}
}
