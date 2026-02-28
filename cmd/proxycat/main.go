package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"proxycatgo/internal/app"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bootstrap, err := app.NewBootstrap("config/config.ini")
	if err != nil {
		log.Fatalf("bootstrap failed: %v", err)
	}

	if err := bootstrap.Run(ctx); err != nil {
		log.Fatalf("runtime failed: %v", err)
	}
}
