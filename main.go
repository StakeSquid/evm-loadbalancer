package main

import (
    "bytes"
    "context"
    "flag"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "math/big"
    "net"
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"
    "path"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/ethereum/go-ethereum/rpc"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "gopkg.in/yaml.v2"
)

type Config struct {
    Port         string          `yaml:"port"`
    LogLevel     string          `yaml:"log_level"`
    LogRateLimit time.Duration   `yaml:"log_rate_limit"`
    Networks     []NetworkConfig `yaml:"networks"`
    MetricsPort  string          `yaml:"metrics_port"`
}

type NodeConfig struct {
    RPCEndpoint        string `yaml:"rpc_endpoint"`
    PrometheusEndpoint string `yaml:"prometheus_endpoint,omitempty"`
}

type NetworkConfig struct {
    Name                           string       `yaml:"name"`
    LocalNodes                     []NodeConfig `yaml:"local_nodes"`
    MonitoringNodes                []NodeConfig `yaml:"monitoring_nodes"`
    FallbackNodes                  []NodeConfig `yaml:"fallback_nodes"`
    LoadBalancePriority            []string     `yaml:"load_balance_priority"`
    LoadPeriod                     int          `yaml:"load_period"`
    LocalPollInterval              string       `yaml:"local_poll_interval"`
    MonitoringPollInterval         string       `yaml:"monitoring_poll_interval"`
    NetworkBlockDiff               int64        `yaml:"network_block_diff"`
    FallbackBlockDiff              int64        `yaml:"fallback_block_diff"` // New field
    UseLoadTracker                 bool         `yaml:"use_load_tracker"`
    RPCTimeout                     string       `yaml:"rpc_timeout"`
    RPCRetries                     int          `yaml:"rpc_retries"`
    SwitchToFallbackEnabled        bool         `yaml:"switch_to_fallback_enabled"`
    SwitchToFallbackBlockThreshold int64        `yaml:"switch_to_fallback_block_threshold"`

    // Parsed durations
    LocalPollIntervalDuration      time.Duration `yaml:"-"`
    MonitoringPollIntervalDuration time.Duration `yaml:"-"`
    RPCTimeoutDuration             time.Duration `yaml:"-"`
}

type NodeStatus struct {
    RPCEndpoint        string
    PrometheusEndpoint string
    Chainhead          *big.Int
    Load               float64
    Latency            time.Duration
    IsLocal            bool
    BlocksBehind       int64
}

type NetworkStatus struct {
    Config                 NetworkConfig
    LocalNodeStatuses      []*NodeStatus
    MonitoringNodeStatuses []*NodeStatus
    FallbackNodeStatuses   []*NodeStatus
    NetworkChainhead       *big.Int
    Mutex                  sync.RWMutex
    CurrentBestEndpoint    string
}

type LoadBalancer struct {
    Config          Config
    Networks        map[string]*NetworkStatus
    LogRateLimit    time.Duration
    LogTimestamps   sync.Map
    ProxyCache      map[string]*httputil.ReverseProxy
    ProxyCacheMutex sync.RWMutex

    // Prometheus metrics
    latencyGauge        *prometheus.GaugeVec
    chainheadGauge      *prometheus.GaugeVec
    blocksBehindGauge   *prometheus.GaugeVec
    loadGauge           *prometheus.GaugeVec
    errorCounter        *prometheus.CounterVec
    bestEndpointGauge   *prometheus.GaugeVec
    requestCounter      *prometheus.CounterVec
    requestDuration     *prometheus.HistogramVec
    requestByIPCounter  *prometheus.CounterVec // New metric

    // Custom Prometheus registry
    promRegistry *prometheus.Registry
}

func logMessage(globalLevel string, level string, format string, args ...interface{}) {
    levels := map[string]int{
        "ERROR": 0,
        "INFO":  1,
        "DEBUG": 2,
        "TRACE": 3,
    }

    if levels[level] <= levels[globalLevel] {
        log.Printf("[%s] %s", level, fmt.Sprintf(format, args...))
    }
}

func NewLoadBalancer(config Config) *LoadBalancer {
    networks := make(map[string]*NetworkStatus)
    for _, networkConfig := range config.Networks {
        localNodeStatuses := make([]*NodeStatus, len(networkConfig.LocalNodes))
        for i, nodeConfig := range networkConfig.LocalNodes {
            localNodeStatuses[i] = &NodeStatus{
                RPCEndpoint:        nodeConfig.RPCEndpoint,
                PrometheusEndpoint: nodeConfig.PrometheusEndpoint,
                Chainhead:          big.NewInt(0),
                Load:               0,
                Latency:            0,
                IsLocal:            true,
            }
        }

        monitoringNodeStatuses := make([]*NodeStatus, len(networkConfig.MonitoringNodes))
        for i, nodeConfig := range networkConfig.MonitoringNodes {
            monitoringNodeStatuses[i] = &NodeStatus{
                RPCEndpoint: nodeConfig.RPCEndpoint,
                Chainhead:   big.NewInt(0),
                IsLocal:     false,
            }
        }

        fallbackNodeStatuses := make([]*NodeStatus, len(networkConfig.FallbackNodes))
        for i, nodeConfig := range networkConfig.FallbackNodes {
            fallbackNodeStatuses[i] = &NodeStatus{
                RPCEndpoint: nodeConfig.RPCEndpoint,
                Chainhead:   big.NewInt(0),
                IsLocal:     false,
            }
        }

        networks[networkConfig.Name] = &NetworkStatus{
            Config:                 networkConfig,
            LocalNodeStatuses:      localNodeStatuses,
            MonitoringNodeStatuses: monitoringNodeStatuses,
            FallbackNodeStatuses:   fallbackNodeStatuses,
            NetworkChainhead:       big.NewInt(0),
        }
    }

    lb := &LoadBalancer{
        Config:          config,
        Networks:        networks,
        LogRateLimit:    config.LogRateLimit,
        ProxyCache:      make(map[string]*httputil.ReverseProxy),
        promRegistry:    prometheus.NewRegistry(), // Initialize custom registry
    }

    lb.initMetrics()

    return lb
}

func (lb *LoadBalancer) initMetrics() {
    lb.latencyGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_node_latency_seconds",
            Help: "Latency to nodes",
        },
        []string{"network", "rpc_endpoint"},
    )

    lb.chainheadGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_node_chainhead",
            Help: "Chainhead of nodes",
        },
        []string{"network", "rpc_endpoint"},
    )

    lb.blocksBehindGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_node_blocks_behind",
            Help: "Number of blocks a node is behind the network chainhead",
        },
        []string{"network", "rpc_endpoint"},
    )

    lb.loadGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_node_load",
            Help: "Load of nodes",
        },
        []string{"network", "rpc_endpoint"},
    )

    lb.errorCounter = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "loadbalancer_node_errors_total",
            Help: "Total errors encountered",
        },
        []string{"network", "rpc_endpoint", "type"},
    )

    lb.bestEndpointGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_best_endpoint",
            Help: "Indicates the best endpoint selected (1 for selected, 0 otherwise)",
        },
        []string{"network", "rpc_endpoint"},
    )

    lb.requestCounter = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "loadbalancer_requests_total",
            Help: "Total number of requests",
        },
        []string{"network"},
    )

    lb.requestDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "loadbalancer_request_duration_seconds",
            Help:    "Histogram of request durations",
            Buckets: prometheus.DefBuckets,
        },
        []string{"network"},
    )

    // New metric for requests by client IP
    lb.requestByIPCounter = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "loadbalancer_requests_by_ip_total",
            Help: "Total number of requests by client IP",
        },
        []string{"network", "client_ip"},
    )

    // Register only the custom metrics with the custom registry
    lb.promRegistry.MustRegister(lb.latencyGauge)
    lb.promRegistry.MustRegister(lb.chainheadGauge)
    lb.promRegistry.MustRegister(lb.blocksBehindGauge)
    lb.promRegistry.MustRegister(lb.loadGauge)
    lb.promRegistry.MustRegister(lb.errorCounter)
    lb.promRegistry.MustRegister(lb.bestEndpointGauge)
    lb.promRegistry.MustRegister(lb.requestCounter)
    lb.promRegistry.MustRegister(lb.requestDuration)
    lb.promRegistry.MustRegister(lb.requestByIPCounter) // Register new metric
}

func (lb *LoadBalancer) logRateLimited(level string, key string, format string, args ...interface{}) {
    now := time.Now()

    lastLoggedInterface, _ := lb.LogTimestamps.Load(key)
    lastLogged, _ := lastLoggedInterface.(time.Time)

    if lastLogged.IsZero() || now.Sub(lastLogged) >= lb.LogRateLimit {
        lb.LogTimestamps.Store(key, now)
        logMessage(lb.Config.LogLevel, level, format, args...)
    }
}

func (lb *LoadBalancer) Start() {
    for _, networkStatus := range lb.Networks {
        ns := networkStatus // capture variable
        go lb.startNetworkMonitoring(ns)
        go lb.startEndpointSelection(ns)
    }

    // Start Prometheus metrics server
    go lb.startMetricsServer()
}

func (lb *LoadBalancer) startMetricsServer() {
    // Create a new ServeMux for the metrics server
    metricsMux := http.NewServeMux()
    // Use the custom registry in the Prometheus handler
    metricsMux.Handle("/metrics", promhttp.HandlerFor(lb.promRegistry, promhttp.HandlerOpts{}))

    logMessage(lb.Config.LogLevel, "INFO", "Starting metrics server on port %s", lb.Config.MetricsPort)
    log.Fatal(http.ListenAndServe(":"+lb.Config.MetricsPort, metricsMux))
}

func (lb *LoadBalancer) startNetworkMonitoring(ns *NetworkStatus) {
    // Monitor local nodes
    for _, node := range ns.LocalNodeStatuses {
        node := node // capture variable
        go lb.monitorNode(ns, node, ns.Config.LocalPollIntervalDuration)
    }

    // Monitor monitoring nodes
    for _, node := range ns.MonitoringNodeStatuses {
        node := node // capture variable
        go lb.monitorNode(ns, node, ns.Config.MonitoringPollIntervalDuration)
    }

    // Monitor fallback nodes
    for _, node := range ns.FallbackNodeStatuses {
        node := node // capture variable
        go lb.monitorNode(ns, node, ns.Config.MonitoringPollIntervalDuration)
    }
}

func (lb *LoadBalancer) startEndpointSelection(ns *NetworkStatus) {
    ticker := time.NewTicker(5 * time.Second) // Adjust the interval as needed
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            lb.GetBestEndpoint(ns)
        }
    }
}

func (lb *LoadBalancer) monitorNode(ns *NetworkStatus, node *NodeStatus, pollInterval time.Duration) {
    for {
        client, err := rpc.Dial(node.RPCEndpoint)
        if err != nil {
            lb.logRateLimited("ERROR", "rpc_dial_error_"+node.RPCEndpoint, "Network %s: Failed to dial RPC endpoint %s: %v", ns.Config.Name, node.RPCEndpoint, err)
            lb.errorCounter.WithLabelValues(ns.Config.Name, node.RPCEndpoint, "dial_error").Inc()
            time.Sleep(pollInterval)
            continue
        }

        // Get chainhead and latency
        chainhead, latency, err := lb.getChainheadWithRetries(client, ns.Config.RPCTimeoutDuration, ns.Config.RPCRetries)
        client.Close()
        if err != nil {
            lb.logRateLimited("ERROR", "chainhead_error_"+node.RPCEndpoint, "Network %s: Failed to get chainhead from %s: %v", ns.Config.Name, node.RPCEndpoint, err)
            lb.errorCounter.WithLabelValues(ns.Config.Name, node.RPCEndpoint, "chainhead_error").Inc()
            time.Sleep(pollInterval)
            continue
        } else {
            node.Chainhead = chainhead
            node.Latency = latency
            lb.logRateLimited("DEBUG", "chainhead_update_"+node.RPCEndpoint, "Network %s: Updated chainhead for RPC endpoint %s: %s, latency: %v", ns.Config.Name, node.RPCEndpoint, chainhead.String(), latency)
        }

        // Update network chainhead
        ns.Mutex.Lock()
        if chainhead.Cmp(big.NewInt(0)) > 0 && (ns.NetworkChainhead == nil || chainhead.Cmp(ns.NetworkChainhead) > 0) {
            ns.NetworkChainhead = chainhead
        }
        ns.Mutex.Unlock()

        // Compute blocks behind
        ns.Mutex.RLock()
        networkChainhead := new(big.Int).Set(ns.NetworkChainhead)
        ns.Mutex.RUnlock()

        blocksBehind := new(big.Int).Sub(networkChainhead, node.Chainhead).Int64()
        if blocksBehind < 0 {
            blocksBehind = 0
        }
        node.BlocksBehind = blocksBehind

        lb.logRateLimited("DEBUG", "blocks_behind_"+node.RPCEndpoint, "Network %s: Node %s is %d blocks behind", ns.Config.Name, node.RPCEndpoint, blocksBehind)

        // Fetch server load if enabled and node is local
        if node.IsLocal && ns.Config.UseLoadTracker && node.PrometheusEndpoint != "" {
            load, err := getServerLoad(node.PrometheusEndpoint, ns.Config.LoadPeriod) // Pass the LoadPeriod here
            if err != nil {
                lb.logRateLimited("ERROR", "load_error_"+node.RPCEndpoint, "Network %s: Failed to get load from %s: %v", ns.Config.Name, node.PrometheusEndpoint, err)
                lb.errorCounter.WithLabelValues(ns.Config.Name, node.RPCEndpoint, "load_error").Inc()
            } else {
                node.Load = load
                lb.logRateLimited("DEBUG", "load_update_"+node.RPCEndpoint, "Network %s: Updated load for RPC endpoint %s: %.2f", ns.Config.Name, node.RPCEndpoint, load)
                lb.loadGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(load)
            }
        }

        // Update Prometheus metrics
        lb.latencyGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(latency.Seconds())
        lb.chainheadGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(float64(chainhead.Int64()))
        lb.blocksBehindGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(float64(blocksBehind))

        time.Sleep(pollInterval)
    }
}

func (lb *LoadBalancer) getChainheadWithRetries(client *rpc.Client, timeout time.Duration, retries int) (*big.Int, time.Duration, error) {
    var chainheadHex string
    var err error
    var latency time.Duration

    for i := 0; i < retries; i++ {
        ctx, cancel := context.WithTimeout(context.Background(), timeout)
        startTime := time.Now()
        err = client.CallContext(ctx, &chainheadHex, "eth_blockNumber")
        latency = time.Since(startTime)
        cancel()

        if err == nil && len(chainheadHex) >= 2 {
            chainheadInt := new(big.Int)
            chainheadInt.SetString(chainheadHex[2:], 16)
            return chainheadInt, latency, nil
        }
        time.Sleep(500 * time.Millisecond)
    }
    return nil, 0, err
}

func (lb *LoadBalancer) GetBestEndpoint(ns *NetworkStatus) string {
    lb.logRateLimited("DEBUG", "starting_endpoint_selection_"+ns.Config.Name, "Starting endpoint selection process for network %s", ns.Config.Name)

    var bestEndpoint string
    var selectionReason string
    highestChainhead := big.NewInt(0)
    lowestLatency := time.Duration(1<<63 - 1) // Max duration
    lowestLoad := float64(1<<63 - 1)          // Arbitrary large number

    ns.Mutex.RLock()
    networkChainhead := new(big.Int).Set(ns.NetworkChainhead)
    ns.Mutex.RUnlock()

    // Compute localNetworkChainhead
    localNetworkChainhead := big.NewInt(0)
    for _, node := range ns.LocalNodeStatuses {
        if node.Chainhead != nil && node.Chainhead.Cmp(localNetworkChainhead) > 0 {
            localNetworkChainhead = node.Chainhead
        }
    }

    // Determine if we should skip local nodes
    skipLocalNodes := false
    allLocalNodesUnreachable := true
    for _, node := range ns.LocalNodeStatuses {
        if node.Chainhead != nil && node.Chainhead.Sign() > 0 {
            allLocalNodesUnreachable = false
            break
        }
    }

    if allLocalNodesUnreachable {
        lb.logRateLimited("INFO", "all_local_nodes_unreachable_"+ns.Config.Name, "All local nodes are unreachable for network %s. Switching to fallback nodes.", ns.Config.Name)
        skipLocalNodes = true
    } else if ns.Config.SwitchToFallbackEnabled {
        chainDiff := new(big.Int).Sub(networkChainhead, localNetworkChainhead)
        chainDiff.Abs(chainDiff) // Take absolute value
        if chainDiff.Int64() > ns.Config.SwitchToFallbackBlockThreshold {
            lb.logRateLimited("INFO", "local_nodes_behind_"+ns.Config.Name, "Local nodes are %d blocks behind network chainhead for network %s. Switching to fallback nodes.", chainDiff.Int64(), ns.Config.Name)
            skipLocalNodes = true
        }
    }

    endpointTypes := []struct {
        nodes       []*NodeStatus
        description string
        blockDiff   int64
    }{}

    if !skipLocalNodes {
        endpointTypes = append(endpointTypes, struct {
            nodes       []*NodeStatus
            description string
            blockDiff   int64
        }{ns.LocalNodeStatuses, "local", ns.Config.NetworkBlockDiff})
    }

    // Add fallback nodes
    endpointTypes = append(endpointTypes, struct {
        nodes       []*NodeStatus
        description string
        blockDiff   int64
    }{ns.FallbackNodeStatuses, "fallback", ns.Config.FallbackBlockDiff})

    for _, endpointType := range endpointTypes {
        nodesToConsider := endpointType.nodes
        if len(nodesToConsider) == 0 {
            continue
        }

        // Compute the highest chainhead among nodesToConsider
        endpointChainhead := big.NewInt(0)
        for _, node := range nodesToConsider {
            if node.Chainhead != nil && node.Chainhead.Cmp(endpointChainhead) > 0 {
                endpointChainhead = node.Chainhead
            }
        }

        validEndpoints := lb.getValidEndpoints(nodesToConsider, endpointChainhead, endpointType.blockDiff)
        if len(validEndpoints) == 0 {
            continue
        }

        // Initial selection based on chainhead
        for _, node := range validEndpoints {
            if node.Chainhead.Cmp(highestChainhead) > 0 {
                highestChainhead = node.Chainhead
                bestEndpoint = node.RPCEndpoint
                selectionReason = fmt.Sprintf("highest chainhead (%s)", highestChainhead.String())
            } else if node.Chainhead.Cmp(highestChainhead) == 0 {
                if node.IsLocal {
                    // Prioritize local nodes
                    bestEndpoint = node.RPCEndpoint
                    selectionReason = "local node preferred"
                }
            }
        }

        // Further refine selection based on load balancing priority
        if bestEndpoint != "" && ns.Config.LoadBalancePriority != nil {
            for _, priority := range ns.Config.LoadBalancePriority {
                if priority == "latency" {
                    for _, node := range validEndpoints {
                        if node.Chainhead.Cmp(highestChainhead) == 0 && node.Latency < lowestLatency {
                            lowestLatency = node.Latency
                            bestEndpoint = node.RPCEndpoint
                            selectionReason = fmt.Sprintf("lowest latency (%v)", lowestLatency)
                        }
                    }
                } else if priority == "load" {
                    for _, node := range validEndpoints {
                        if node.IsLocal && node.Chainhead.Cmp(highestChainhead) == 0 && node.Load < lowestLoad {
                            lowestLoad = node.Load
                            bestEndpoint = node.RPCEndpoint
                            selectionReason = fmt.Sprintf("lowest load (%.2f)", lowestLoad)
                        }
                    }
                }
            }
        }

        if bestEndpoint != "" {
            lb.logRateLimited("INFO", "best_endpoint_"+ns.Config.Name, "Best endpoint selected for network %s: %s (%s)", ns.Config.Name, bestEndpoint, selectionReason)

            // Update Prometheus metrics
            // Reset all endpoints to 0
            for _, node := range ns.LocalNodeStatuses {
                lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(0)
            }
            for _, node := range ns.FallbackNodeStatuses {
                lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, node.RPCEndpoint).Set(0)
            }
            // Set the selected endpoint to 1
            lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, bestEndpoint).Set(1)

            // Store the best endpoint in the network status
            ns.Mutex.Lock()
            ns.CurrentBestEndpoint = bestEndpoint
            ns.Mutex.Unlock()

            return bestEndpoint
        }
    }

    // If no valid endpoint was found in any category
    lb.logRateLimited("ERROR", "no_valid_endpoint_"+ns.Config.Name, "No valid endpoint selected for network %s", ns.Config.Name)

    // Clear the current best endpoint since none is valid
    ns.Mutex.Lock()
    ns.CurrentBestEndpoint = ""
    ns.Mutex.Unlock()

    return ""
}

func (lb *LoadBalancer) getValidEndpoints(nodes []*NodeStatus, endpointChainhead *big.Int, blockDiff int64) []*NodeStatus {
    validEndpoints := []*NodeStatus{}
    for _, node := range nodes {
        if node.Chainhead == nil {
            continue
        }
        blockDiffBig := new(big.Int).Sub(endpointChainhead, node.Chainhead)
        blockDiffBig.Abs(blockDiffBig)
        diff := blockDiffBig.Int64()
        if diff > blockDiff {
            continue // Node is too far behind or ahead
        }
        validEndpoints = append(validEndpoints, node)
    }
    return validEndpoints
}

// getClientIP extracts the client IP address from the request.
func getClientIP(r *http.Request) string {
    // Try to get the IP from the X-Forwarded-For header
    xForwardedFor := r.Header.Get("X-Forwarded-For")
    if xForwardedFor != "" {
        // X-Forwarded-For can be a comma-separated list of IPs
        ips := strings.Split(xForwardedFor, ",")
        // Return the first IP
        ip := strings.TrimSpace(ips[0])
        if ip != "" {
            return ip
        }
    }
    // Fallback to RemoteAddr
    ip, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil {
        return r.RemoteAddr
    }
    return ip
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    startTime := time.Now()
    pathSegments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
    if len(pathSegments) == 0 || pathSegments[0] == "" {
        http.Error(w, "Network name not specified in URL", http.StatusBadRequest)
        return
    }
    networkName := pathSegments[0]
    networkStatus, exists := lb.Networks[networkName]
    if !exists {
        http.Error(w, "Unknown network: "+networkName, http.StatusNotFound)
        return
    }

    // Get the current best endpoint from the network status
    networkStatus.Mutex.RLock()
    bestEndpoint := networkStatus.CurrentBestEndpoint
    networkStatus.Mutex.RUnlock()

    if bestEndpoint == "" {
        http.Error(w, "No valid endpoint available for network "+networkName, http.StatusInternalServerError)
        return
    }

    // Get or create the reverse proxy for the best endpoint
    proxy := lb.getReverseProxy(bestEndpoint, networkName, pathSegments)
    if proxy == nil {
        http.Error(w, "Failed to create reverse proxy", http.StatusInternalServerError)
        return
    }

    // Extract the client IP
    clientIP := getClientIP(r)

    // Log the client IP at TRACE level, obeying the rate limit
    lb.logRateLimited("TRACE", "client_ip_"+clientIP+"_"+networkName, "Client IP %s made a request to network %s", clientIP, networkName)

    // Increment request counter
    lb.requestCounter.WithLabelValues(networkName).Inc()

    // Increment request counter by client IP
    lb.requestByIPCounter.WithLabelValues(networkName, clientIP).Inc()

    // Serve the request using the proxy
    proxy.ServeHTTP(w, r)

    duration := time.Since(startTime).Seconds()
    lb.requestDuration.WithLabelValues(networkName).Observe(duration)
}

func (lb *LoadBalancer) getReverseProxy(endpoint string, networkName string, pathSegments []string) *httputil.ReverseProxy {
    lb.ProxyCacheMutex.RLock()
    proxy, exists := lb.ProxyCache[endpoint]
    lb.ProxyCacheMutex.RUnlock()

    if exists {
        return proxy
    }

    parsedURL, err := url.Parse(endpoint)
    if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
        lb.logRateLimited("ERROR", "invalid_endpoint_url_"+networkName, "Invalid RPC endpoint URL: %s", endpoint)
        lb.errorCounter.WithLabelValues(networkName, endpoint, "invalid_url").Inc()
        return nil
    }

    // Create a custom HTTP transport with connection reuse settings
    transport := &http.Transport{
        Proxy:                 http.ProxyFromEnvironment,
        DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
        MaxIdleConns:          1000,
        MaxIdleConnsPerHost:   1000,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
    }

    // Create a reverse proxy with the custom transport
    proxy = httputil.NewSingleHostReverseProxy(parsedURL)
    proxy.Transport = transport

    // Customize the director function
    proxy.Director = func(req *http.Request) {
        // Set the scheme and host
        req.URL.Scheme = parsedURL.Scheme
        req.URL.Host = parsedURL.Host

        // Modify the path to include any path segments after the network name
        if len(pathSegments) > 1 {
            req.URL.Path = path.Join(parsedURL.Path, strings.Join(pathSegments[1:], "/"))
        } else {
            req.URL.Path = parsedURL.Path
        }

        // Set the Host header to the backend host
        req.Host = parsedURL.Host

        // Ensure the request body can be read multiple times
        if req.GetBody == nil && req.Body != nil {
            bodyBytes, err := ioutil.ReadAll(req.Body)
            if err == nil {
                req.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
                req.GetBody = func() (io.ReadCloser, error) {
                    return ioutil.NopCloser(bytes.NewBuffer(bodyBytes)), nil
                }
            }
        }
    }

    // Optionally, capture and log errors from the backend
    proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
        lb.logRateLimited("ERROR", "proxy_error_"+networkName, "Proxy error for network %s: %v", networkName, err)
        lb.errorCounter.WithLabelValues(networkName, endpoint, "proxy_error").Inc()
        http.Error(w, "Proxy error: "+err.Error(), http.StatusBadGateway)
    }

    // Cache the proxy
    lb.ProxyCacheMutex.Lock()
    lb.ProxyCache[endpoint] = proxy
    lb.ProxyCacheMutex.Unlock()

    return proxy
}

func getServerLoad(prometheusEndpoint string, loadPeriod int) (float64, error) {
    resp, err := http.Get(prometheusEndpoint)
    if err != nil {
        return 0.0, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return 0.0, fmt.Errorf("received non-200 response code: %d", resp.StatusCode)
    }
    bodyBytes, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return 0.0, err
    }
    bodyString := string(bodyBytes)

    // Select the appropriate metric name based on the LoadPeriod
    var loadMetricName string
    switch loadPeriod {
    case 1:
        loadMetricName = "node_load1"
    case 5:
        loadMetricName = "node_load5"
    case 15:
        loadMetricName = "node_load15"
    default:
        return 0.0, fmt.Errorf("invalid LoadPeriod: %d", loadPeriod)
    }

    // Parse the metrics to find the load metric
    lines := strings.Split(bodyString, "\n")
    for _, line := range lines {
        if strings.HasPrefix(line, loadMetricName+" ") {
            fields := strings.Fields(line)
            if len(fields) != 2 {
                continue
            }
            loadValue, err := strconv.ParseFloat(fields[1], 64)
            if err != nil {
                continue
            }
            return loadValue, nil
        }
    }
    return 0.0, fmt.Errorf("%s metric not found", loadMetricName)
}

func main() {
    configFile := flag.String("config", "config.yaml", "Path to configuration YAML file")
    flag.Parse()

    file, err := os.Open(*configFile)
    if err != nil {
        log.Fatalf("Failed to open config file: %v", err)
    }
    defer file.Close()

    decoder := yaml.NewDecoder(file)
    var config Config
    err = decoder.Decode(&config)
    if err != nil {
        log.Fatalf("Failed to decode config file: %v", err)
    }

    // Parse durations from strings
    for i, network := range config.Networks {
        localPollInterval, err := time.ParseDuration(network.LocalPollInterval)
        if err != nil {
            log.Fatalf("Invalid local_poll_interval for network %s: %v", network.Name, err)
        }
        config.Networks[i].LocalPollIntervalDuration = localPollInterval

        monitoringPollInterval, err := time.ParseDuration(network.MonitoringPollInterval)
        if err != nil {
            log.Fatalf("Invalid monitoring_poll_interval for network %s: %v", network.Name, err)
        }
        config.Networks[i].MonitoringPollIntervalDuration = monitoringPollInterval

        rpcTimeout, err := time.ParseDuration(network.RPCTimeout)
        if err != nil {
            log.Fatalf("Invalid rpc_timeout for network %s: %v", network.Name, err)
        }
        config.Networks[i].RPCTimeoutDuration = rpcTimeout
    }

    // Set default metrics port if not specified
    if config.MetricsPort == "" {
        config.MetricsPort = "9101"
    }

    lb := NewLoadBalancer(config)

    lb.Start()

    // Use a new ServeMux for the main server
    mainMux := http.NewServeMux()
    mainMux.HandleFunc("/", lb.ServeHTTP)
    logMessage(config.LogLevel, "INFO", "Starting load balancer on port %s", config.Port)
    log.Fatal(http.ListenAndServe(":"+config.Port, mainMux))
}
