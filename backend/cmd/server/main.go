package main

import (
	"flag"
	"log"
	"os"

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
		port = "6050"
	}

	log.Printf("Rclone Manager starting on port %s", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
