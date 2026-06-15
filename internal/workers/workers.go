package workers

import (
	"context"
	"log/slog"
	"time"
)

// shutdownTimeout bounds the whole ordered shutdown so a stuck worker (e.g. a
// hung peer fetch or a wedged subprocess) cannot block process exit forever.
const shutdownTimeout = 30 * time.Second

// Workers manages all background work with ordered shutdown.
type Workers struct {
	Services   []Service
	Waiters    []Waiter
	Drainables []Service
	Cleanup    []Stoppable
	Logger     *slog.Logger
}

func (w *Workers) Start(ctx context.Context) {
	for _, svc := range w.Services {
		svc.Start(ctx)
	}
	for _, svc := range w.Drainables {
		svc.Start(ctx)
	}
}

// Stop shuts down all workers, bounded by shutdownTimeout so a wedged worker
// cannot hang process exit.
func (w *Workers) Stop() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.stop()
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		w.Logger.Warn("background shutdown exceeded deadline, forcing exit", "timeout", shutdownTimeout)
	}
}

// stop shuts down all workers in four ordered phases:
// services, waiters, drainables, cleanup.
func (w *Workers) stop() {
	w.Logger.Info("stopping background services")
	for _, svc := range w.Services {
		svc.Stop()
	}

	w.Logger.Info("waiting for in-flight work to drain")
	for _, waiter := range w.Waiters {
		waiter.Wait()
	}

	w.Logger.Info("stopping drainable services")
	for _, svc := range w.Drainables {
		svc.Stop()
	}

	w.Logger.Info("cleaning up caches and resources")
	for _, c := range w.Cleanup {
		c.Stop()
	}
}
