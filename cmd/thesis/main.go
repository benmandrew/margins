// Command thesis is the Typst review server.
//
//	thesis serve        run the server (the default)
//	thesis healthcheck  exit 0 when the local server answers /healthz
//
// The config file is THESIS_CONFIG, or /etc/thesis/config.toml.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// The image has no zoneinfo; the timers need the configured zone.
	_ "time/tzdata"

	"git.benmandrew.com/me/thesis/internal/config"
	"git.benmandrew.com/me/thesis/internal/service"
	"git.benmandrew.com/me/thesis/internal/store"
	"git.benmandrew.com/me/thesis/internal/web"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q; use serve or healthcheck", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "thesis:", err)
		os.Exit(1)
	}
}

func configPath() string {
	if p := os.Getenv("THESIS_CONFIG"); p != "" {
		return p
	}
	return "/etc/thesis/config.toml"
}

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "db.sqlite"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	svc := service.New(cfg, st, secrets, log)
	if err := svc.Start(ctx); err != nil {
		return err
	}
	srv, err := web.New(svc, secrets)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	workers := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(workers)
	}()
	// A fetch at boot catches pushes made while the service was down.
	if err := svc.EnqueueFetch(ctx); err != nil {
		log.Error("queuing the boot fetch", "err", err)
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "origin", cfg.Origin)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		stop()
		<-workers
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = httpSrv.Shutdown(shutdownCtx)
	// Run returns once its workers have stopped; a job cut short is reset
	// to queued at the next boot.
	<-workers
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func healthcheck() error {
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", res.Status)
	}
	return nil
}
