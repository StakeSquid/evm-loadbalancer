package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/evm-loadbalancer/internal/loadbalancer"
	"github.com/sirupsen/logrus"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "Path to configuration file")
	flag.Parse()

	if configPath == "" {
		fmt.Fprintf(os.Stderr, "Configuration file path is required\n")
		os.Exit(1)
	}

	lb, err := loadbalancer.New(configPath)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to initialize load balancer")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logrus.Info("Received shutdown signal")
		cancel()
	}()

	if err := lb.Start(ctx); err != nil {
		logrus.WithError(err).Fatal("Load balancer failed")
	}

	logrus.Info("Load balancer stopped")
}