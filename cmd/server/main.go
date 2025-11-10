package main

import (
	"context"
	"log"
	"os"

	"bakery/pkg/app"
)

// main acts as a thin adapter so existing process managers can keep using cmd/server.
func main() {
	logger := log.New(os.Stdout, "[bakery] ", log.LstdFlags)
	if err := app.Run(context.Background(), os.Args[1:], logger); err != nil {
		logger.Fatalf("application stopped with error: %v", err)
	}
}
