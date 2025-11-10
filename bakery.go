package main

import (
	"context"
	"log"
	"os"

	"bakery/pkg/app"
)

// main exposes a root-level entry point so operators can simply run `go run bakery.go`.
func main() {
	logger := log.New(os.Stdout, "[bakery] ", log.LstdFlags)
	if err := app.Run(context.Background(), os.Args[1:], logger); err != nil {
		logger.Fatalf("application stopped with error: %v", err)
	}
}
