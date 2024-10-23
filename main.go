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
    "strings"
    "sync"
    "time"

    "github.com/ethereum/go-ethereum/rpc"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "gopkg.in/yaml.v2"
)

type Config struct {
    Port             string          `yaml:"port"`
    LogLevel         string          `yaml:"log_level"`
    LogRateLimit     time.Duration   `yaml:"log_rate_limit"`
    Networks         []NetworkConfig `yaml:"networks"`
    MetricsPort      string          `yaml:"metrics_port"`
}

type NetworkConfig struct {
    Name                            string   `yaml:"name"`
    LocalEndpoints                  []string `yaml:"local_endpoints"`
    MonitoringEndpoints             []string `yaml:"monitoring_endpoints"`
    FallbackEndpoints               []string `yaml:"fallback_endpoints"`
    LoadBalancePriority             []string `yaml:"load_balance_priority"`
    LoadPeriod                      int      `yaml:"load_period"`
    LocalPollInterval               string   `yaml:"local_poll_interval"`
    MonitoringPollInterval          string   `yaml:"monitoring_poll_interval"`
    NetworkBlockDiff                int64    `yaml:"network_block_diff"`
    UseLoadTracker                  bool     `yaml:"use_load_tracker"`
    RPCTimeout                      string   `yaml:"rpc_timeout"`
    RPCRetries                      int      `yaml:"rpc_retries"`
    SwitchToFallbackEnabled         bool     `yaml:"switch_to_fallback_enabled"`
    SwitchToFallbackBlockThreshold  int64    `yaml:"switch_to_fallback_block_threshold"`

    // Parsed durations
    LocalPollIntervalDuration      time.Duration `yaml:"-"`
    MonitoringPollIntervalDuration time.Duration `yaml:"-"`
    RPCTimeoutDuration             time.Duration `yaml:"-"`
}


type NodeStatus struct {
    Endpoint   string
    Chainhead  *big.Int
    Load       float64
    Latency    time.Duration
    IsLocal    bool
}

type NetworkStatus struct {
    Config                   NetworkConfig
    LocalNodeStatuses        []*NodeStatus
    MonitoringNodeStatuses   []*NodeStatus
    FallbackNodeStatuses     []*NodeStatus
    NetworkChainhead         *big.Int
    Mutex                    sync.RWMutex
}

type LoadBalancer struct {
    Config          Config
    Networks        map[string]*NetworkStatus
    LogRateLimit    time.Duration
    LogTimestamps   sync.Map
    ProxyCache      map[string]*httputil.ReverseProxy
    ProxyCacheMutex sync.RWMutex

    // Prometheus metrics
    latencyGauge      *prometheus.GaugeVec
    chainheadGauge    *prometheus.GaugeVec
    errorCounter      *prometheus.CounterVec
    bestEndpointGauge *prometheus.GaugeVec
    requestCounter    *prometheus.CounterVec
    requestDuration   *prometheus.HistogramVec

    // Custom Prometheus registry
    promRegistry *prometheus.Registry
}

func logMessage(globalLevel string, level string, format string, args ...interface{}) {
    levels := map[string]int{
        "ERROR": 0,
        "INFO":  1,
        "DEBUG": 2,
    }

    if levels[level] <= levels[globalLevel] {
        log.Printf("[%s] %s", level, fmt.Sprintf(format, args...))
    }
}

func NewLoadBalancer(config Config) *LoadBalancer {
    networks := make(map[string]*NetworkStatus)
    for _, networkConfig := range config.Networks {
        localNodeStatuses := make([]*NodeStatus, len(networkConfig.LocalEndpoints))
        for i, endpoint := range networkConfig.LocalEndpoints {
            localNodeStatuses[i] = &NodeStatus{
                Endpoint:  endpoint,
                Chainhead: big.NewInt(0),
                Load:      0,
                Latency:   0,
                IsLocal:   true,
            }
        }

        monitoringNodeStatuses := make([]*NodeStatus, len(networkConfig.MonitoringEndpoints))
        for i, endpoint := range networkConfig.MonitoringEndpoints {
            monitoringNodeStatuses[i] = &NodeStatus{
                Endpoint:  endpoint,
                Chainhead: big.NewInt(0),
                IsLocal:   false,
            }
        }

        fallbackNodeStatuses := make([]*NodeStatus, len(networkConfig.FallbackEndpoints))
        for i, endpoint := range networkConfig.FallbackEndpoints {
            fallbackNodeStatuses[i] = &NodeStatus{
                Endpoint:  endpoint,
                Chainhead: big.NewInt(0),
                IsLocal:   false,
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
        []string{"network", "endpoint"},
    )

    lb.chainheadGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_node_chainhead",
            Help: "Chainhead of nodes",
        },
        []string{"network", "endpoint"},
    )

    lb.errorCounter = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "loadbalancer_node_errors_total",
            Help: "Total errors encountered",
        },
        []string{"network", "endpoint", "type"},
    )

    lb.bestEndpointGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "loadbalancer_best_endpoint",
            Help: "Indicates the best endpoint selected (1 for selected, 0 otherwise)",
        },
        []string{"network", "endpoint"},
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

    // Register only the custom metrics with the custom registry
    lb.promRegistry.MustRegister(lb.latencyGauge)
    lb.promRegistry.MustRegister(lb.chainheadGauge)
    lb.promRegistry.MustRegister(lb.errorCounter)
    lb.promRegistry.MustRegister(lb.bestEndpointGauge)
    lb.promRegistry.MustRegister(lb.requestCounter)
    lb.promRegistry.MustRegister(lb.requestDuration)
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
    var wg sync.WaitGroup

    // Monitor local nodes
    for _, node := range ns.LocalNodeStatuses {
        node := node // capture variable
        wg.Add(1)
        go func() {
            defer wg.Done()
            lb.monitorNode(ns, node, ns.Config.LocalPollIntervalDuration)
        }()
    }

    // Monitor monitoring nodes
    for _, node := range ns.MonitoringNodeStatuses {
        node := node // capture variable
        wg.Add(1)
        go func() {
            defer wg.Done()
            lb.monitorNode(ns, node, ns.Config.MonitoringPollIntervalDuration)
        }()
    }

    // Monitor fallback nodes
    for _, node := range ns.FallbackNodeStatuses {
        node := node // capture variable
        wg.Add(1)
        go func() {
            defer wg.Done()
            lb.monitorNode(ns, node, ns.Config.MonitoringPollIntervalDuration)
        }()
    }

    wg.Wait()
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
        client, err := rpc.Dial(node.Endpoint)
        if err != nil {
            lb.logRateLimited("ERROR", "rpc_dial_error_"+node.Endpoint, "Network %s: Failed to dial endpoint %s: %v", ns.Config.Name, node.Endpoint, err)
            lb.errorCounter.WithLabelValues(ns.Config.Name, node.Endpoint, "dial_error").Inc()
            time.Sleep(pollInterval)
            continue
        }

        // Get chainhead and latency
        chainhead, latency, err := lb.getChainheadWithRetries(client, ns.Config.RPCTimeoutDuration, ns.Config.RPCRetries)
        client.Close()
        if err != nil {
            lb.logRateLimited("ERROR", "chainhead_error_"+node.Endpoint, "Network %s: Failed to get chainhead from %s: %v", ns.Config.Name, node.Endpoint, err)
            lb.errorCounter.WithLabelValues(ns.Config.Name, node.Endpoint, "chainhead_error").Inc()
            time.Sleep(pollInterval)
            continue
        } else {
            node.Chainhead = chainhead
            node.Latency = latency
            lb.logRateLimited("DEBUG", "chainhead_update_"+node.Endpoint, "Network %s: Updated chainhead for endpoint %s: %s, latency: %v", ns.Config.Name, node.Endpoint, chainhead.String(), latency)
        }

        // Update network chainhead
        ns.Mutex.Lock()
        if ns.NetworkChainhead == nil || chainhead.Cmp(ns.NetworkChainhead) > 0 {
            ns.NetworkChainhead = chainhead
        }
        ns.Mutex.Unlock()

        // Update Prometheus metrics
        lb.latencyGauge.WithLabelValues(ns.Config.Name, node.Endpoint).Set(latency.Seconds())
        lb.chainheadGauge.WithLabelValues(ns.Config.Name, node.Endpoint).Set(float64(chainhead.Int64()))

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

        if err == nil {
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
        if chainDiff.Int64() > ns.Config.SwitchToFallbackBlockThreshold {
            lb.logRateLimited("INFO", "local_nodes_behind_"+ns.Config.Name, "Local nodes are %d blocks behind network chainhead for network %s. Switching to fallback nodes.", chainDiff.Int64(), ns.Config.Name)
            skipLocalNodes = true
        }
    }

    endpointTypes := []struct {
        nodes            []*NodeStatus
        description      string
        networkChainhead *big.Int
    }{}

    if !skipLocalNodes {
        endpointTypes = append(endpointTypes, struct {
            nodes            []*NodeStatus
            description      string
            networkChainhead *big.Int
        }{ns.LocalNodeStatuses, "local", localNetworkChainhead})
    }

    endpointTypes = append(endpointTypes, struct {
        nodes            []*NodeStatus
        description      string
        networkChainhead *big.Int
    }{ns.FallbackNodeStatuses, "fallback", networkChainhead})

    for _, endpointType := range endpointTypes {
        nodesToConsider := endpointType.nodes
        if len(nodesToConsider) == 0 {
            continue
        }

        validEndpoints := lb.getValidEndpoints(nodesToConsider, endpointType.networkChainhead, ns.Config.NetworkBlockDiff)
        if len(validEndpoints) == 0 {
            continue
        }

        // Initial selection based on chainhead
        for _, node := range validEndpoints {
            if node.Chainhead.Cmp(highestChainhead) > 0 {
                highestChainhead = node.Chainhead
                bestEndpoint = node.Endpoint
                selectionReason = fmt.Sprintf("highest chainhead (%s)", highestChainhead.String())
            } else if node.Chainhead.Cmp(highestChainhead) == 0 {
                if node.IsLocal {
                    // Prioritize local nodes
                    bestEndpoint = node.Endpoint
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
                            bestEndpoint = node.Endpoint
                            selectionReason = fmt.Sprintf("lowest latency (%v)", lowestLatency)
                        }
                    }
                } else if priority == "load" {
                    for _, node := range validEndpoints {
                        if node.Chainhead.Cmp(highestChainhead) == 0 && node.Load < lowestLoad {
                            lowestLoad = node.Load
                            bestEndpoint = node.Endpoint
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
                lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, node.Endpoint).Set(0)
            }
            for _, node := range ns.FallbackNodeStatuses {
                lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, node.Endpoint).Set(0)
            }
            // Set the selected endpoint to 1
            lb.bestEndpointGauge.WithLabelValues(ns.Config.Name, bestEndpoint).Set(1)

            return bestEndpoint
        }
    }

    // If no valid endpoint was found in any category
    lb.logRateLimited("ERROR", "no_valid_endpoint_"+ns.Config.Name, "No valid endpoint selected for network %s", ns.Config.Name)
    return ""
}


func (lb *LoadBalancer) getValidEndpoints(nodes []*NodeStatus, networkChainhead *big.Int, networkBlockDiff int64) []*NodeStatus {
    validEndpoints := []*NodeStatus{}
    for _, node := range nodes {
        if node.Chainhead == nil {
            continue
        }
        blockDiff := new(big.Int).Sub(networkChainhead, node.Chainhead).Int64()
        if blockDiff > networkBlockDiff {
            continue // Node is too far behind
        }
        validEndpoints = append(validEndpoints, node)
    }
    return validEndpoints
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

    bestEndpoint := lb.GetBestEndpoint(networkStatus)

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

    // Increment request counter
    lb.requestCounter.WithLabelValues(networkName).Inc()

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
        lb.logRateLimited("ERROR", "invalid_endpoint_url_"+networkName, "Invalid endpoint URL: %s", endpoint)
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

func getServerLoad(nodeExporterEndpoint string, loadPeriod int) (float64, error) {
    // For simplicity, return a dummy load
    return 0.0, nil
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
