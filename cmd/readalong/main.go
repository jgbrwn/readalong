package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/httpapp"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

func main() {
	cfg := config.Load()
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()
	worker := pipeline.New(cfg, d)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()
	srv := &http.Server{
		Addr: cfg.Addr, Handler: httpapp.New(cfg, d, worker),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("readalong listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("readalong server stopped: %v", err)
	}
	stop()
	<-workerDone
}
