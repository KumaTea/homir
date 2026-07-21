package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/server"
)

func main() {
	configPath := flag.String("config", "homir.yaml", "path to the Homir configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	app, err := server.New(context.Background(), cfg, slog.Default())
	if err != nil {
		slog.Error("create server", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	slog.Info("Homir listening", "address", cfg.ListenAddress)
	if err := app.ListenAndServe(); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}
