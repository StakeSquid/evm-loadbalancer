# EVM Load Balancer

A high-performance, configurable load balancer for Ethereum Virtual Machine (EVM) compatible blockchain nodes with support for HTTP and WebSocket connections.

## Features

- **Multi-Network Support**: Configure multiple blockchain networks with independent node pools
- **Dynamic Load Balancing**: Multiple strategies including chainhead-based, latency-based, load-based, and least connections
- **Protocol Support**: Both HTTP/HTTPS and WebSocket/WSS connections
- **Automatic Failover**: Seamlessly switches between load balancing nodes and fallback nodes
- **Node Health Monitoring**: Continuous monitoring of node health, block height, and latency
- **Prometheus Metrics**: Comprehensive metrics for monitoring and alerting
- **Rate-Limited Logging**: Prevents log flooding while maintaining visibility
- **Connection Pooling**: Efficient connection reuse for better performance

## Architecture

The load balancer consists of several modular components:

- **Config Manager**: Handles YAML configuration parsing and validation
- **Node Monitor**: Continuously polls nodes for health, chainhead, and performance metrics
- **Endpoint Selector**: Implements load balancing algorithms to select the best node
- **Proxy Manager**: Handles transparent HTTP and WebSocket proxying
- **Metrics Collector**: Exposes Prometheus metrics for monitoring
- **Rate Limiter**: Prevents log flooding for recurring errors

## Installation

```bash
git clone https://github.com/your-org/evm-loadbalancer
cd evm-loadbalancer
go mod download
go build -o evm-lb cmd/loadbalancer/main.go
```

## Configuration

Create a `config.yaml` file based on the example in `configs/config.example.yaml`:

```yaml
server:
  port: 8080
  metrics_port: 9101
  log_level: info

rate_limiting:
  error_log_interval: 1m

networks:
  - name: mainnet
    poll_interval: 5s
    timeout: 10s
    retry_count: 3
    block_diff_threshold: 10
    selection_interval: 5s
    load_balancing_strategy:
      - chainhead
      - latency
    
    load_balancing_nodes:
      - url: http://localhost:8545
      - url: ws://localhost:8546
    
    reference_nodes:
      - url: https://mainnet.infura.io/v3/YOUR_PROJECT_ID
    
    fallback_nodes:
      - url: https://cloudflare-eth.com
```

## Running

```bash
./evm-lb -config config.yaml
```

## Usage

Once running, the load balancer exposes endpoints for each configured network:

- HTTP requests: `http://localhost:8080/<network-name>/<json-rpc-path>`
- WebSocket connections: `ws://localhost:8080/<network-name>/`
- Metrics: `http://localhost:9101/metrics`

Example:
```bash
# HTTP JSON-RPC request
curl -X POST http://localhost:8080/mainnet/ \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'

# WebSocket connection
wscat -c ws://localhost:8080/mainnet/
```

## Metrics

The load balancer exposes the following Prometheus metrics:

- `evm_lb_node_latency_ms`: Node response latency in milliseconds
- `evm_lb_node_chainhead`: Current block number of each node
- `evm_lb_node_blocks_behind`: How many blocks a node is behind the network
- `evm_lb_node_healthy`: Node health status (1=healthy, 0=unhealthy)
- `evm_lb_selected_endpoint`: Currently selected best endpoint per network
- `evm_lb_http_requests_total`: Total HTTP requests by network and status
- `evm_lb_websocket_connections_active`: Active WebSocket connections
- And more...

## Load Balancing Strategies

Configure strategies in order of priority:

1. **chainhead**: Prefers nodes with the highest block number
2. **latency**: Prefers nodes with the lowest response time
3. **load**: Prefers nodes with the lowest system load
4. **least_connection**: Prefers nodes with fewer active connections

## Node Types

- **Load Balancing Nodes**: Primary nodes for serving traffic
- **Reference Nodes**: Used to determine the true network chainhead
- **Fallback Nodes**: Backup nodes used when primary nodes are unhealthy

## Development

```bash
# Run tests
go test ./...

# Run with race detector
go run -race cmd/loadbalancer/main.go -config config.yaml

# Build for production
go build -ldflags="-s -w" -o evm-lb cmd/loadbalancer/main.go
```

## License

MIT