package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/config"
	"github.com/bruwbird/lcp/apps/openai-serve/internal/httpapi"
	"github.com/bruwbird/lcp/apps/openai-serve/internal/lcpd"
	"github.com/gin-gonic/gin"
)

const (
	dialTimeout       = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeoutSlack = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

func main() {
	os.Exit(realMain())
}

const (
	exitOK          = 0
	exitError       = 1
	exitConfigError = 2
)

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitConfigError
	}

	logLevel, err := parseLogLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level: %v\n", err)
		return exitConfigError
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	if logLevel <= slog.LevelDebug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	ctxDial, cancelDial := context.WithTimeout(ctx, dialTimeout)
	defer cancelDial()

	lcpdClient, err := lcpd.Dial(ctxDial, cfg.LCPDGRPCAddr)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to lcpd-grpcd", "addr", cfg.LCPDGRPCAddr, "err", err)
		return exitConfigError
	}
	defer func() {
		if closeErr := lcpdClient.Close(); closeErr != nil {
			logger.ErrorContext(ctx, "failed to close lcpd client", "err", closeErr)
		}
	}()

	api, err := httpapi.New(cfg, lcpdClient.Service(), logger)
	if err != nil {
		logger.ErrorContext(ctx, "failed to initialize http server", "err", err)
		return exitConfigError
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      cfg.TimeoutExecute + writeTimeoutSlack,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	logger.InfoContext(ctx, "listening", "addr", cfg.HTTPAddr)

	select {
	case srvErr := <-errCh:
		if srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
			logger.ErrorContext(ctx, "server error", "err", srvErr)
			return exitError
		}
	case <-ctx.Done():
		logger.InfoContext(ctx, "shutdown signal received", "err", ctx.Err())
	}

	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	shutdownErr := srv.Shutdown(ctxShutdown)
	if shutdownErr != nil {
		logger.ErrorContext(ctxShutdown, "shutdown failed", "err", shutdownErr)
	}

	srvErr := <-errCh
	if srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
		logger.ErrorContext(ctx, "server error after shutdown", "err", srvErr)
		return exitError
	}

	return exitOK
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level: %q", s)
	}
}
