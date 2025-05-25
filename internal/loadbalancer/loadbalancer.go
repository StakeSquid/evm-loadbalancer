package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/evm-loadbalancer/internal/config"
	"github.com/evm-loadbalancer/internal/logger"
	"github.com/evm-loadbalancer/internal/metrics"
	"github.com/evm-loadbalancer/internal/node"
	"github.com/evm-loadbalancer/internal/proxy"
	"github.com/evm-loadbalancer/internal/selector"
	"github.com/evm-loadbalancer/internal/types"
	"github.com/sirupsen/logrus"
)

type LoadBalancer struct {
	config      *config.Manager
	networks    map[string]*types.NetworkStatus
	proxy       *proxy.ProxyManager
	metrics     *metrics.Collector
	logger      *logrus.Logger
	rateLimiter *logger.RateLimiter
	server      *http.Server
	metricsPort int
}

func New(configMgr *config.Manager) (*LoadBalancer, error) {
	if configMgr == nil {
		return nil, fmt.Errorf("config manager is nil")
	}

	cfg := configMgr.GetConfig()

	log, err := logger.SetupLogger(cfg.Server.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to setup logger: %w", err)
	}

	errorLogInterval, err := time.ParseDuration(cfg.RateLimiting.ErrorLogInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid error log interval: %w", err)
	}

	rateLimiter := logger.NewRateLimiter(errorLogInterval, log)

	networks, err := configMgr.InitializeNetworkStatus()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize network status: %w", err)
	}

	metricsCollector := metrics.NewCollector()

	proxyMgr := proxy.NewProxyManager(networks, log.WithField("component", "proxy"), rateLimiter)

	addr := ""
	if cfg.Server.Port > 0 {
		addr = fmt.Sprintf(":%d", cfg.Server.Port)
	}

	server := &http.Server{
		Addr:         addr,
		Handler:      proxyMgr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return &LoadBalancer{
		config:      configMgr,
		networks:    networks,
		proxy:       proxyMgr,
		metrics:     metricsCollector,
		logger:      log,
		rateLimiter: rateLimiter,
		server:      server,
		metricsPort: cfg.Server.MetricsPort,
	}, nil
}

func (lb *LoadBalancer) Start(ctx context.Context) error {
	lb.logger.Info("Starting EVM Load Balancer")

	go metrics.StartMetricsServer(lb.metricsPort, lb.metrics, lb.logger.WithField("component", "metrics"))

	var wg sync.WaitGroup

	for _, netConfig := range lb.config.GetNetworkConfigs() {
		networkStatus := lb.networks[netConfig.Name]

		pollInterval, _ := time.ParseDuration(netConfig.PollInterval)
		timeout, _ := time.ParseDuration(netConfig.Timeout)
		selectionInterval, _ := time.ParseDuration(netConfig.SelectionInterval)

		allNodes := append(append(networkStatus.LoadBalancingNodes,
			networkStatus.ReferenceNodes...),
			networkStatus.FallbackNodes...)

		for _, n := range allNodes {
			wg.Add(1)
			go func(nodeStatus *types.NodeStatus) {
				defer wg.Done()
				monitor := node.NewMonitor(nodeStatus, pollInterval, timeout, netConfig.RetryCount,
					lb.logger.WithField("component", "monitor"), lb.rateLimiter)
				monitor.Start(ctx)
			}(n)
		}

		wg.Add(1)
		go func(ns *types.NetworkStatus, nc types.NetworkConfig) {
			defer wg.Done()
			endpointSelector := selector.NewEndpointSelector(
				ns,
				nc.LoadBalancingStrategy,
				nc.BlockDiffThreshold,
				selectionInterval,
				lb.logger.WithField("component", "selector"),
				lb.rateLimiter,
				lb.metrics,
			)
			endpointSelector.Start(ctx)
		}(networkStatus, netConfig)

		wg.Add(1)
		go func(ns *types.NetworkStatus) {
			defer wg.Done()
			lb.updateMetricsLoop(ctx, ns)
		}(networkStatus)
	}

	errChan := make(chan error, 1)
	go func() {
		lb.logger.WithField("port", lb.server.Addr).Info("Starting HTTP server")
		if err := lb.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		lb.logger.Info("Shutting down load balancer")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := lb.server.Shutdown(shutdownCtx); err != nil {
			lb.logger.WithError(err).Error("Failed to shutdown server gracefully")
		}

		wg.Wait()
		return nil
	case err := <-errChan:
		return fmt.Errorf("server error: %w", err)
	}
}

func (lb *LoadBalancer) updateMetricsLoop(ctx context.Context, network *types.NetworkStatus) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lb.updateNetworkMetrics(network)
		case <-ctx.Done():
			return
		}
	}
}

func (lb *LoadBalancer) updateNetworkMetrics(network *types.NetworkStatus) {
	allNodes := append(append(network.LoadBalancingNodes,
		network.ReferenceNodes...),
		network.FallbackNodes...)

	for _, node := range allNodes {
		lb.metrics.UpdateNodeMetrics(network.Name, node)
	}

	httpBest := network.GetBestEndpoint(types.ProtocolHTTP)
	wsBest := network.GetBestEndpoint(types.ProtocolWebSocket)

	if httpBest != nil {
		lb.metrics.UpdateSelectedEndpoint(network.Name, allNodes, httpBest, types.ProtocolHTTP)
	}

	if wsBest != nil {
		lb.metrics.UpdateSelectedEndpoint(network.Name, allNodes, wsBest, types.ProtocolWebSocket)
	}
}

func (lb *LoadBalancer) Handler() http.Handler {
	return lb.proxy
}
