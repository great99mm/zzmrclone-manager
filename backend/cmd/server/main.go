package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rclone-manager/internal/api"
	"rclone-manager/internal/config"
	"rclone-manager/internal/logger"
)

func main() {
	resetPassword := flag.Bool("reset-password", false, "Reset admin password to a new random value and exit")
	flag.Parse()

	cfg := config.Load()

	if *resetPassword {
		if _, err := api.ResetAdminPassword(cfg.DataDir); err != nil {
			log.Printf("ERROR: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	logger.Init(cfg.LogDir)

	router := api.SetupRouter(cfg)

	port := cfg.Port
	if port == "" {
		port = "7070"
	}

	log.Printf("Rclone Manager starting on port %s", port)
	server := &http.Server{Addr: ":" + port, Handler: router}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serverErr:
		api.ShutdownBackgroundServices()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	case <-signals:
		api.ShutdownBackgroundServices()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Failed to shut down server cleanly: %v", err)
		}
	}
}
