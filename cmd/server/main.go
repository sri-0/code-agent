package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"code-agent/internal/bootstrap"
	"code-agent/internal/server"
)

var version = "dev"

func main() {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	res, err := bootstrap.Init(rootCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
	cfg := res.Cfg
	logger := res.Logger

	if res.Orch != nil {
		go res.Orch.Start(rootCtx)
	}

	router := server.NewRouter(server.Deps{
		Version: version,
		Tasks:   res.Tasks,
		Orch:    res.Orch,
		Logger:  logger,
	})

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info().Str("signal", sig.String()).Msg("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("shutdown error")
		}
		rootCancel()
		if res.Valkey != nil {
			res.Valkey.Close()
		}
	}()

	logger.Info().Str("addr", addr).Msg("server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal().Err(err).Msg("server error")
	}
}
