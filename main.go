package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	})))

	model, err := LoadModel(cfg.ModelPath)
	if err != nil {
		slog.Error("failed to load matter data model", "path", cfg.ModelPath, "err", err)
		os.Exit(2)
	}
	nClusters, nDeviceTypes := model.Size()

	slog.Info("starting matter2mqtt",
		"matter_ws", cfg.MatterWSURL,
		"mqtt_broker", cfg.MQTTBroker,
		"topic_prefix", cfg.TopicPrefix,
		"publish_names", cfg.PublishNames,
		"model_clusters", nClusters,
		"model_device_types", nDeviceTypes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bridge := NewBridge(cfg, model)

	if cfg.HTTPEnabled {
		srv := NewServer(cfg, bridge)
		if err := srv.Start(); err != nil {
			slog.Error("could not start console", "addr", cfg.HTTPAddr, "err", err)
			os.Exit(1)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx)
		}()
	}

	if err := bridge.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("bridge exited", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
