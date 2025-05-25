package types

import (
	"net/url"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type NodeType string

const (
	NodeTypeLoadBalancing NodeType = "load_balancing"
	NodeTypeReference     NodeType = "reference"
	NodeTypeFallback      NodeType = "fallback"
)

type Protocol string

const (
	ProtocolHTTP      Protocol = "http"
	ProtocolHTTPS     Protocol = "https"
	ProtocolWebSocket Protocol = "ws"
	ProtocolWSS       Protocol = "wss"
)

type LoadBalancingStrategy string

const (
	StrategyChainHead       LoadBalancingStrategy = "chainhead"
	StrategyLatency         LoadBalancingStrategy = "latency"
	StrategyLoad            LoadBalancingStrategy = "load"
	StrategyLeastConnection LoadBalancingStrategy = "least_connection"
)

type NodeConfig struct {
	URL                string            `yaml:"url"`
	PrometheusEndpoint string            `yaml:"prometheus_endpoint,omitempty"`
	Priority           int               `yaml:"priority,omitempty"`
	Protocol           Protocol          `yaml:"protocol,omitempty"`
	Labels             map[string]string `yaml:"labels,omitempty"`
}

type NetworkConfig struct {
	Name                  string                  `yaml:"name"`
	LoadBalancingNodes    []NodeConfig            `yaml:"load_balancing_nodes"`
	ReferenceNodes        []NodeConfig            `yaml:"reference_nodes,omitempty"`
	FallbackNodes         []NodeConfig            `yaml:"fallback_nodes,omitempty"`
	PollInterval          string                  `yaml:"poll_interval"`
	Timeout               string                  `yaml:"timeout"`
	RetryCount            int                     `yaml:"retry_count"`
	BlockDiffThreshold    int64                   `yaml:"block_diff_threshold"`
	LoadBalancingStrategy []LoadBalancingStrategy `yaml:"load_balancing_strategy"`
	SelectionInterval     string                  `yaml:"selection_interval"`
}

type Config struct {
	Server struct {
		Port        int    `yaml:"port"`
		MetricsPort int    `yaml:"metrics_port"`
		LogLevel    string `yaml:"log_level"`
	} `yaml:"server"`
	RateLimiting struct {
		ErrorLogInterval string `yaml:"error_log_interval"`
	} `yaml:"rate_limiting"`
	Networks []NetworkConfig `yaml:"networks"`
}

type NodeStatus struct {
	URL          *url.URL
	NodeType     NodeType
	Protocol     Protocol
	ChainHead    int64
	Latency      time.Duration
	Load         float64
	LastUpdate   time.Time
	LastError    error
	ErrorCount   int
	Healthy      bool
	BlocksBehind int64
	mu           sync.RWMutex
}

func (ns *NodeStatus) Update(chainHead int64, latency time.Duration, load float64, err error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	previousHealthy := ns.Healthy
	previousChainHead := ns.ChainHead
	previousErrorCount := ns.ErrorCount

	ns.LastUpdate = time.Now()
	if err != nil {
		ns.LastError = err
		ns.ErrorCount++
		ns.Healthy = false
	} else {
		ns.ChainHead = chainHead
		ns.Latency = latency
		ns.Load = load
		ns.Healthy = true
		ns.ErrorCount = 0
		ns.LastError = nil
	}

	// Log state transitions for debugging race conditions
	if previousHealthy != ns.Healthy {
		logEntry := logrus.WithFields(logrus.Fields{
			"node":               ns.URL.String(),
			"node_type":          ns.NodeType,
			"previous_healthy":   previousHealthy,
			"current_healthy":    ns.Healthy,
			"previous_chainhead": previousChainHead,
			"current_chainhead":  ns.ChainHead,
			"error_count":        ns.ErrorCount,
			"latency_ms":         latency.Milliseconds(),
		})
		if err != nil {
			logEntry = logEntry.WithError(err)
		}
		logEntry.Debug("Node health state transition")
	}

	// Log when error count increases
	if ns.ErrorCount > previousErrorCount {
		logrus.WithFields(logrus.Fields{
			"node":               ns.URL.String(),
			"node_type":          ns.NodeType,
			"previous_errors":    previousErrorCount,
			"current_errors":     ns.ErrorCount,
			"healthy":            ns.Healthy,
		}).WithError(err).Debug("Node error count increased")
	}

	// Log significant chain head changes
	if ns.Healthy && previousChainHead > 0 && chainHead > previousChainHead+10 {
		logrus.WithFields(logrus.Fields{
			"node":               ns.URL.String(),
			"node_type":          ns.NodeType,
			"previous_chainhead": previousChainHead,
			"current_chainhead":  chainHead,
			"jump":               chainHead - previousChainHead,
		}).Debug("Node chain head jumped significantly")
	}
}

func (ns *NodeStatus) GetStatus() (int64, time.Duration, float64, bool, int64) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.ChainHead, ns.Latency, ns.Load, ns.Healthy, ns.BlocksBehind
}

func (ns *NodeStatus) SetBlocksBehind(blocks int64) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	previousBlocksBehind := ns.BlocksBehind
	ns.BlocksBehind = blocks

	// Log significant changes in blocks behind
	if previousBlocksBehind != blocks && (blocks > 5 || previousBlocksBehind > 5) {
		logrus.WithFields(logrus.Fields{
			"node":                  ns.URL.String(),
			"node_type":             ns.NodeType,
			"previous_blocks_behind": previousBlocksBehind,
			"current_blocks_behind":  blocks,
			"healthy":               ns.Healthy,
		}).Debug("Node blocks behind changed")
	}
}

type NetworkStatus struct {
	Name               string
	LoadBalancingNodes []*NodeStatus
	ReferenceNodes     []*NodeStatus
	FallbackNodes      []*NodeStatus
	BestEndpointHTTP   *NodeStatus
	BestEndpointWS     *NodeStatus
	NetworkChainHead   int64
	mu                 sync.RWMutex
}

func (ns *NetworkStatus) SetBestEndpoint(node *NodeStatus, protocol Protocol) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	switch protocol {
	case ProtocolHTTP, ProtocolHTTPS:
		ns.BestEndpointHTTP = node
	case ProtocolWebSocket, ProtocolWSS:
		ns.BestEndpointWS = node
	}
}

func (ns *NetworkStatus) GetBestEndpoint(protocol Protocol) *NodeStatus {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	switch protocol {
	case ProtocolHTTP, ProtocolHTTPS:
		return ns.BestEndpointHTTP
	case ProtocolWebSocket, ProtocolWSS:
		return ns.BestEndpointWS
	default:
		return ns.BestEndpointHTTP
	}
}

func (ns *NetworkStatus) SetNetworkChainHead(chainHead int64) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.NetworkChainHead = chainHead
}

func (ns *NetworkStatus) GetNetworkChainHead() int64 {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.NetworkChainHead
}
