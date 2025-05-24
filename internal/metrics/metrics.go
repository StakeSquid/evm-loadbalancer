package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/evm-loadbalancer/internal/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type Collector struct {
	nodeLatency          *prometheus.GaugeVec
	nodeChainHead        *prometheus.GaugeVec
	nodeBlocksBehind     *prometheus.GaugeVec
	nodeLoad             *prometheus.GaugeVec
	nodeHealthy          *prometheus.GaugeVec
	nodeErrors           *prometheus.CounterVec
	selectedEndpoint     *prometheus.GaugeVec
	httpRequestsTotal    *prometheus.CounterVec
	httpRequestDuration  *prometheus.HistogramVec
	wsConnectionsActive  *prometheus.GaugeVec
	wsConnectionsTotal   *prometheus.CounterVec
	wsConnectionDuration *prometheus.HistogramVec
	registry             *prometheus.Registry
}

func NewCollector() *Collector {
	c := &Collector{
		nodeLatency: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_node_latency_ms",
				Help: "Node latency in milliseconds",
			},
			[]string{"network", "node", "node_type"},
		),
		nodeChainHead: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_node_chainhead",
				Help: "Current chainhead (block number) of the node",
			},
			[]string{"network", "node", "node_type"},
		),
		nodeBlocksBehind: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_node_blocks_behind",
				Help: "Number of blocks the node is behind the network chainhead",
			},
			[]string{"network", "node", "node_type"},
		),
		nodeLoad: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_node_load",
				Help: "Node load (1-minute average)",
			},
			[]string{"network", "node", "node_type"},
		),
		nodeHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_node_healthy",
				Help: "Whether the node is healthy (1) or not (0)",
			},
			[]string{"network", "node", "node_type"},
		),
		nodeErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "evm_lb_node_errors_total",
				Help: "Total number of errors for the node",
			},
			[]string{"network", "node", "node_type", "error_type"},
		),
		selectedEndpoint: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_selected_endpoint",
				Help: "Currently selected best endpoint (1 for selected, 0 for not selected)",
			},
			[]string{"network", "node", "protocol"},
		),
		httpRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "evm_lb_http_requests_total",
				Help: "Total number of HTTP requests",
			},
			[]string{"network", "client_ip", "method", "status"},
		),
		httpRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "evm_lb_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"network", "method"},
		),
		wsConnectionsActive: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "evm_lb_websocket_connections_active",
				Help: "Number of active WebSocket connections",
			},
			[]string{"network", "client_ip"},
		),
		wsConnectionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "evm_lb_websocket_connections_total",
				Help: "Total number of WebSocket connections",
			},
			[]string{"network", "client_ip"},
		),
		wsConnectionDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "evm_lb_websocket_connection_duration_seconds",
				Help:    "WebSocket connection duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"network"},
		),
	}

	c.registry = prometheus.NewRegistry()
	c.registry.MustRegister(
		c.nodeLatency,
		c.nodeChainHead,
		c.nodeBlocksBehind,
		c.nodeLoad,
		c.nodeHealthy,
		c.nodeErrors,
		c.selectedEndpoint,
		c.httpRequestsTotal,
		c.httpRequestDuration,
		c.wsConnectionsActive,
		c.wsConnectionsTotal,
		c.wsConnectionDuration,
	)

	return c
}

func (c *Collector) UpdateNodeMetrics(network string, node *types.NodeStatus) {
	chainHead, latency, load, healthy, blocksBehind := node.GetStatus()

	labels := prometheus.Labels{
		"network":   network,
		"node":      node.URL.String(),
		"node_type": string(node.NodeType),
	}

	c.nodeChainHead.With(labels).Set(float64(chainHead))
	c.nodeLatency.With(labels).Set(float64(latency.Milliseconds()))
	c.nodeLoad.With(labels).Set(load)
	c.nodeBlocksBehind.With(labels).Set(float64(blocksBehind))

	if healthy {
		c.nodeHealthy.With(labels).Set(1)
	} else {
		c.nodeHealthy.With(labels).Set(0)
	}
}

func (c *Collector) UpdateSelectedEndpoint(network string, nodes []*types.NodeStatus, selected *types.NodeStatus, protocol types.Protocol) {
	for _, node := range nodes {
		labels := prometheus.Labels{
			"network":  network,
			"node":     node.URL.String(),
			"protocol": string(protocol),
		}

		if node == selected {
			c.selectedEndpoint.With(labels).Set(1)
		} else {
			c.selectedEndpoint.With(labels).Set(0)
		}
	}
}

func (c *Collector) IncrementNodeError(network string, node *types.NodeStatus, errorType string) {
	c.nodeErrors.With(prometheus.Labels{
		"network":    network,
		"node":       node.URL.String(),
		"node_type":  string(node.NodeType),
		"error_type": errorType,
	}).Inc()
}

func (c *Collector) RecordHTTPRequest(network, clientIP, method string, status int, duration time.Duration) {
	labels := prometheus.Labels{
		"network":   network,
		"client_ip": clientIP,
		"method":    method,
		"status":    strconv.Itoa(status),
	}

	c.httpRequestsTotal.With(labels).Inc()
	c.httpRequestDuration.With(prometheus.Labels{
		"network": network,
		"method":  method,
	}).Observe(duration.Seconds())
}

func (c *Collector) IncrementWebSocketConnection(network, clientIP string) {
	c.wsConnectionsTotal.With(prometheus.Labels{
		"network":   network,
		"client_ip": clientIP,
	}).Inc()

	c.wsConnectionsActive.With(prometheus.Labels{
		"network":   network,
		"client_ip": clientIP,
	}).Inc()
}

func (c *Collector) DecrementWebSocketConnection(network, clientIP string, duration time.Duration) {
	c.wsConnectionsActive.With(prometheus.Labels{
		"network":   network,
		"client_ip": clientIP,
	}).Dec()

	c.wsConnectionDuration.With(prometheus.Labels{
		"network": network,
	}).Observe(duration.Seconds())
}

func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}

func StartMetricsServer(port int, collector *Collector, logger *logrus.Entry) {
	if port == 0 {
		logger.Info("Metrics server disabled (port=0)")
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", collector.Handler())

	addr := ":" + strconv.Itoa(port)
	logger.WithField("port", port).Info("Starting metrics server")

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.WithError(err).Error("Failed to start metrics server")
	}
}
