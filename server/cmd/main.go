package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/api"
	"sparkdb/scaletozero/internals/docker"
	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/manager"
	"sparkdb/scaletozero/internals/proxy"
	"sparkdb/scaletozero/internals/store"
)

func main() {
	logger := log.New(os.Stdout, "[sparkdb] ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config error: %v", err)
	}

	st, err := store.Open(cfg.SQLITE_PATH)
	if err != nil {
		logger.Fatalf("store error: %v", err)
	}
	defer st.Close()

	// Engines are registered here; the rest of the system is engine agnostic.
	engines := engine.NewRegistry(&engine.Postgres{})

	dockerClient := docker.New(cfg)

	mgr, err := manager.New(cfg, dockerClient, engines, st, logger)
	if err != nil {
		logger.Fatalf("manager error: %v", err)
	}

	// Cancelled on the first SIGINT/SIGTERM, which unwinds every subsystem.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := newHTTPServer(cfg, mgr, engines)

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// Control plane.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Printf("admin API listening on %s", cfg.PORT)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Data plane: one TCP proxy per engine that shares a single port.
	pgEngine, _ := engines.Get("postgres")
	pgProxy := proxy.New(pgEngine, mgr, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := pgProxy.ListenAndServe(ctx, cfg.PG_LISTEN_ADDR); err != nil {
			errCh <- err
		}
	}()

	// Idle reaper.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Printf("reaper running: idle timeout %s, scan every %s", cfg.IDLE_TIMEOUT, cfg.REAP_INTERVAL)
		mgr.RunReaper(ctx)
	}()

	select {
	case <-ctx.Done():
		logger.Println("shutdown signal received")
	case err := <-errCh:
		logger.Printf("subsystem failed: %v", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http shutdown: %v", err)
	}

	wg.Wait()
	logger.Println("stopped")
}

// newHTTPServer builds the control-plane server.
func newHTTPServer(cfg *config.Config, mgr *manager.Manager, engines engine.Registry) *http.Server {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	api.New(mgr, engines).Register(r)

	return &http.Server{
		Addr:              cfg.PORT,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
}
