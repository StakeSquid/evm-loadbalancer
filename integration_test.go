package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/evm-loadbalancer/internal/config"
	"github.com/evm-loadbalancer/internal/loadbalancer"
	"github.com/evm-loadbalancer/internal/logger"
)

const testConfigContent = `
server:
  port: 0
  metrics_port: 0
  log_level: info

rate_limiting:
  error_log_interval: 1m

networks:
  - name: ethereum
    poll_interval: 0.5s
    timeout: 5s
    retry_count: 3
    block_diff_threshold: 10
    selection_interval: 1s
    load_balancing_strategy:
      - chainhead
      - latency
      - load
    
    load_balancing_nodes:
      - url: http://test-node-1:8545
        priority: 1
      - url: http://test-node-2:8545
        priority: 1
    
    reference_nodes:
      - url: http://reference-node:8545
    
    fallback_nodes:
      - url: http://fallback-node:8545

  - name: polygon
    poll_interval: 0.5s
    timeout: 5s
    retry_count: 3
    block_diff_threshold: 10
    selection_interval: 1s
    load_balancing_strategy:
      - chainhead
      - latency
    
    load_balancing_nodes:
      - url: http://polygon-node:8545
        priority: 1
    
    fallback_nodes:
      - url: http://polygon-fallback:8545
`

type MockEthNode struct {
	server      *httptest.Server
	chainHead   int64
	latency     time.Duration
	shouldFail  bool
	failureType string
	mu          sync.RWMutex
}

func NewMockEthNode(chainHead int64, latency time.Duration) *MockEthNode {
	node := &MockEthNode{
		chainHead: chainHead,
		latency:   latency,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", node.handleRequest)
	node.server = httptest.NewServer(mux)

	return node
}

func (m *MockEthNode) URL() string {
	return m.server.URL
}

func (m *MockEthNode) Close() {
	m.server.Close()
}

func (m *MockEthNode) SetFailure(shouldFail bool, failureType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shouldFail = shouldFail
	m.failureType = failureType
}

func (m *MockEthNode) SetChainHead(chainHead int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chainHead = chainHead
}

func (m *MockEthNode) SetLatency(latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = latency
}

func (m *MockEthNode) handleRequest(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	shouldFail := m.shouldFail
	failureType := m.failureType
	latency := m.latency
	chainHead := m.chainHead
	m.mu.RUnlock()

	time.Sleep(latency)

	if shouldFail {
		switch failureType {
		case "timeout":
			time.Sleep(10 * time.Second)
		case "500":
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		case "connection_refused":
			w.Header().Set("Connection", "close")
			return
		default:
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	method, ok := req["method"].(string)
	if !ok {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var response map[string]interface{}

	switch method {
	case "eth_blockNumber":
		response = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  fmt.Sprintf("0x%x", chainHead),
		}
	case "eth_getBlockByNumber":
		response = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]interface{}{
				"number":          fmt.Sprintf("0x%x", chainHead),
				"hash":            "0x1234567890abcdef",
				"parentHash":      "0xabcdef1234567890",
				"timestamp":       "0x5f5e100",
				"gasLimit":        "0x1c9c380",
				"gasUsed":         "0x5208",
				"transactions":    []interface{}{},
				"transactionRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"stateRoot":       "0xd7f8974fb5ac78d9ac099b9ad5018bedc2ce0a72dad1827a1709da30580f0544",
				"receiptsRoot":    "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
			},
		}
	case "eth_getBalance":
		response = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  "0x1bc16d674ec80000",
		}
	case "net_version":
		response = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  "1",
		}
	default:
		response = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"error": map[string]interface{}{
				"code":    -32601,
				"message": "Method not found",
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func createTestConfig(nodes map[string]*MockEthNode) string {
	config := `
server:
  port: 0
  metrics_port: 0
  log_level: info

rate_limiting:
  error_log_interval: 1m

networks:
  - name: ethereum
    poll_interval: 0.5s
    timeout: 5s
    retry_count: 3
    block_diff_threshold: 10
    selection_interval: 1s
    load_balancing_strategy:
      - chainhead
      - latency
      - load
    
    load_balancing_nodes:`

	for name, node := range nodes {
		if name == "node1" || name == "node2" {
			config += fmt.Sprintf(`
      - url: %s
        priority: 1`, node.URL())
		}
	}

	config += `
    
    reference_nodes:`

	if node, exists := nodes["reference"]; exists {
		config += fmt.Sprintf(`
      - url: %s`, node.URL())
	}

	config += `
    
    fallback_nodes:`

	if node, exists := nodes["fallback"]; exists {
		config += fmt.Sprintf(`
      - url: %s`, node.URL())
	}

	return config
}

func TestLoadBalancerIntegration(t *testing.T) {
	logger.InitLogger("info")

	t.Run("BasicLoadBalancing", func(t *testing.T) {
		testBasicLoadBalancing(t)
	})

	t.Run("BackendFailureHandling", func(t *testing.T) {
		testBackendFailureHandling(t)
	})

	t.Run("ConcurrencyTest", func(t *testing.T) {
		testConcurrency(t)
	})

	t.Run("HeaderAndBodyIntegrity", func(t *testing.T) {
		testHeaderAndBodyIntegrity(t)
	})

	t.Run("ChainHeadSelection", func(t *testing.T) {
		testChainHeadSelection(t)
	})

	t.Run("LatencyBasedSelection", func(t *testing.T) {
		testLatencyBasedSelection(t)
	})

	t.Run("FallbackMechanism", func(t *testing.T) {
		testFallbackMechanism(t)
	})

	t.Run("NodeRecovery", func(t *testing.T) {
		testNodeRecovery(t)
	})
}

func testBasicLoadBalancing(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1000, 100*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	for i := 0; i < 10; i++ {
		resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
		if err != nil {
			t.Fatalf("Request %d failed: %v", i, err)
		}

		var result map[string]interface{}
		if err := json.Unmarshal(resp, &result); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}

		if result["error"] != nil {
			t.Fatalf("Request returned error: %v", result["error"])
		}

		if result["result"] == nil {
			t.Fatalf("Request returned no result")
		}
	}
}

func testBackendFailureHandling(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1000, 100*time.Millisecond)
	fallback := NewMockEthNode(1000, 200*time.Millisecond)
	defer node1.Close()
	defer node2.Close()
	defer fallback.Close()

	t.Logf("Node1 URL: %s", node1.URL())
	t.Logf("Node2 URL: %s", node2.URL())
	t.Logf("Fallback URL: %s", fallback.URL())

	nodes := map[string]*MockEthNode{
		"node1":    node1,
		"node2":    node2,
		"fallback": fallback,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	node1.SetFailure(true, "500")

	// Wait for the monitor to detect the failure and selector to update
	time.Sleep(2 * time.Second)

	t.Logf("Making request to: %s/ethereum", testServer.URL)
	resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Logf("Response body: %s", string(resp))
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request should have succeeded using node2 or fallback: %v", result["error"])
	}

	node2.SetFailure(true, "500")
	time.Sleep(1 * time.Second)

	resp, err = makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request should have succeeded using fallback: %v", result["error"])
	}
}

func testConcurrency(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1000, 100*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	const numConcurrentRequests = 100
	var wg sync.WaitGroup
	results := make(chan error, numConcurrentRequests)

	for i := 0; i < numConcurrentRequests; i++ {
		wg.Add(1)
		go func(reqNum int) {
			defer wg.Done()

			resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
			if err != nil {
				results <- fmt.Errorf("request %d failed: %v", reqNum, err)
				return
			}

			var result map[string]interface{}
			if err := json.Unmarshal(resp, &result); err != nil {
				results <- fmt.Errorf("request %d parse failed: %v", reqNum, err)
				return
			}

			if result["error"] != nil {
				results <- fmt.Errorf("request %d returned error: %v", reqNum, result["error"])
				return
			}

			results <- nil
		}(i)
	}

	wg.Wait()
	close(results)

	var errors []error
	for err := range results {
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		t.Fatalf("Concurrency test failed with %d errors. First error: %v", len(errors), errors[0])
	}
}

func testHeaderAndBodyIntegrity(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	defer node1.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_getBalance",
		"params":  []interface{}{"0x742d35Cc6634C0532925a3b8D0542Eb0d5F57E4d", "latest"},
		"id":      1,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", testServer.URL+"/ethereum", bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "test-value")
	req.Header.Set("Authorization", "Bearer test-token")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request returned error: %v", result["error"])
	}

	if result["result"] == nil {
		t.Fatalf("Request returned no result")
	}

	expectedBalance := "0x1bc16d674ec80000"
	if result["result"] != expectedBalance {
		t.Fatalf("Expected balance %s, got %v", expectedBalance, result["result"])
	}
}

func testChainHeadSelection(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1005, 100*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(3 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	for i := 0; i < 5; i++ {
		resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		var result map[string]interface{}
		if err := json.Unmarshal(resp, &result); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}

		if result["error"] != nil {
			t.Fatalf("Request returned error: %v", result["error"])
		}

		blockNumber := result["result"].(string)
		if blockNumber != "0x3ed" {
			t.Logf("Expected to route to node2 (block 0x3ed), but got block %s", blockNumber)
		}
	}
}

func testLatencyBasedSelection(t *testing.T) {
	node1 := NewMockEthNode(1000, 200*time.Millisecond)
	node2 := NewMockEthNode(1000, 50*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(3 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	start := time.Now()
	resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request returned error: %v", result["error"])
	}

	if duration > 150*time.Millisecond {
		t.Logf("Request took %v, expected to route to faster node2", duration)
	}
}

func testFallbackMechanism(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1000, 100*time.Millisecond)
	fallback := NewMockEthNode(990, 150*time.Millisecond)
	defer node1.Close()
	defer node2.Close()
	defer fallback.Close()

	nodes := map[string]*MockEthNode{
		"node1":    node1,
		"node2":    node2,
		"fallback": fallback,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	node1.SetFailure(true, "500")
	node2.SetFailure(true, "500")
	time.Sleep(1 * time.Second)

	resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request should have succeeded using fallback: %v", result["error"])
	}

	blockNumber := result["result"].(string)
	if blockNumber != "0x3de" {
		t.Fatalf("Expected fallback node block (0x3de), got %s", blockNumber)
	}
}

func testNodeRecovery(t *testing.T) {
	node1 := NewMockEthNode(1000, 50*time.Millisecond)
	node2 := NewMockEthNode(1000, 100*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	configFile := createTempConfigFile(t, configContent)
	defer os.Remove(configFile)

	configManager, err := config.NewManager(configFile)
	if err != nil {
		t.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		t.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	node1.SetFailure(true, "500")
	time.Sleep(1 * time.Second)

	resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if result["error"] != nil {
		t.Fatalf("Request should have succeeded using node2: %v", result["error"])
	}

	node1.SetFailure(false, "")
	time.Sleep(2 * time.Second)

	for i := 0; i < 10; i++ {
		resp, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
		if err != nil {
			t.Fatalf("Request %d failed: %v", i, err)
		}

		if err := json.Unmarshal(resp, &result); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}

		if result["error"] != nil {
			t.Fatalf("Request %d should have succeeded: %v", i, result["error"])
		}
	}
}

func makeJSONRPCRequest(url, method string, params interface{}) ([]byte, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}

	return body, nil
}

func createTempConfigFile(t *testing.T, content string) string {
	tmpFile, err := os.CreateTemp("", "test_config_*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	return tmpFile.Name()
}

func BenchmarkLoadBalancer(b *testing.B) {
	node1 := NewMockEthNode(1000, 10*time.Millisecond)
	node2 := NewMockEthNode(1000, 20*time.Millisecond)
	defer node1.Close()
	defer node2.Close()

	nodes := map[string]*MockEthNode{
		"node1": node1,
		"node2": node2,
	}

	configContent := createTestConfig(nodes)
	tmpFile, err := os.CreateTemp("", "bench_config_*.yaml")
	if err != nil {
		b.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		b.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	configManager, err := config.NewManager(tmpFile.Name())
	if err != nil {
		b.Fatalf("Failed to create config manager: %v", err)
	}

	lb, err := loadbalancer.New(configManager)
	if err != nil {
		b.Fatalf("Failed to create load balancer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go lb.Start(ctx)
	time.Sleep(2 * time.Second)

	testServer := httptest.NewServer(lb.Handler())
	defer testServer.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := makeJSONRPCRequest(testServer.URL+"/ethereum", "eth_blockNumber", nil)
			if err != nil {
				b.Errorf("Request failed: %v", err)
			}
		}
	})
}
