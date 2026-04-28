package main

import (
	"log"

	"rclone-manager/internal/api"
	"rclone-manager/internal/config"
	"rclone-manager/internal/logger"
)

func main() {
	cfg := config.Load()

	logger.Init(cfg.LogDir)

	router := api.SetupRouter(cfg)

	port := cfg.Port
	if port == "" {
		port = "7070"
	}

	log.Printf("Rclone Manager starting on port %s", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
