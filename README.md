# StakeSquid EVM Load Balancer

StakeSquid EVM Load Balancer is a high-performance tool designed to efficiently distribute traffic across EVM nodes, ensuring optimal performance, redundancy, and high availability. It intelligently selects the best node based on real-time metrics like block height, latency, and server load, with automatic failover to backup nodes when needed. Featuring Prometheus integration for comprehensive monitoring, this load balancer provides a robust solution for querying Ethereum networks across multiple environments.

## Features
- Load balancing across Ethereum nodes based on multiple factors (chainhead, latency, load)
- Supports multiple networks
- Prometheus integration for metrics collection
- Fallback mechanisms for node failures
- Rate-limited logging to reduce log noise
- Simple YAML-based configuration

# How it works?
## Overview

The StakeSquid EVM Load Balancer is designed to efficiently manage requests to Ethereum nodes across multiple networks by balancing load, monitoring node health, and ensuring high availability through failover mechanisms. It continuously monitors nodes, collects metrics, and selects the best node for routing based on configurable factors such as latency, load, and chainhead synchronization.

The core logic behind the load balancer is to provide a reliable and performant infrastructure for querying Ethereum nodes, even in the presence of node failures or network issues.

## Core Components

### 1. Node Monitoring
The load balancer continuously monitors the nodes (local, monitoring, and fallback) to collect real-time data about their health. This includes:

- **Chainhead**: The current block number reported by the node.
- **Latency**: The time taken to respond to requests.
- **Load** (optional): The server load, typically collected using external tools like node exporters.

Each node is periodically polled based on the `local_poll_interval` and `monitoring_poll_interval` defined in the configuration. If a node becomes unresponsive or lags behind the network's chainhead, it will be deprioritized.

### 2. Load Balancing Logic
The core of the system's decision-making revolves around selecting the best node to route requests to based on multiple factors. These factors can be prioritized in the configuration, including:

- **Chainhead**: Nodes that are most in sync with the network are preferred. Nodes with higher block numbers are considered more up-to-date.
  
- **Latency**: Nodes with lower response times are prioritized for faster request handling.

- **Load**: If load tracking is enabled, nodes with lower resource usage are preferred to balance the load and avoid overloading a single node.

#### Selection Process:

1. **Chainhead Validation**: 
   - The load balancer checks if nodes are within an acceptable block difference (`network_block_diff`) from the network chainhead. Nodes that are too far behind are excluded.
   
2. **Initial Selection**: 
   - Nodes with the highest chainhead are selected first. If multiple nodes have the same chainhead, the load balancer will further prioritize based on the configured load balancing priority (latency or load).

3. **Refinement by Latency and Load**: 
   - If two or more nodes have the same chainhead, the balancer selects the one with the lowest latency or load, based on the priority defined in the configuration (`load_balance_priority`).

4. **Local Node Prioritization**: 
   - If a local node (i.e., a node in the `local_nodes`) is up-to-date and responsive, it is generally prioritized over monitoring or fallback nodes.

### 3. Failover Mechanism

Failover is a key part of the system’s resilience. The load balancer categorizes nodes into three types:

- **Local Endpoints**: These are the primary nodes, typically located in the same network environment as the balancer.
- **Monitoring Endpoints** (optional): These are external nodes used for monitoring purposes.
- **Fallback Endpoints** (optional): These are the final fallback nodes in case all local nodes are down or not in sync.

The balancer will failover to monitoring or fallback nodes if:

- A local node is behind in blocks (beyond the `network_block_diff`), is slow, or unresponsive.
- Monitoring nodes are also considered only when local nodes fail, and fallback nodes are considered if neither local nor monitoring nodes are suitable.

The failover process happens automatically without interruption in service, ensuring high availability. Once a previously failed node recovers, it can be reintroduced into the pool of valid nodes.

### 4. Prometheus Metrics

Prometheus metrics are used to monitor the load balancer's performance and the health of the nodes. Metrics are exposed on a dedicated `/metrics` endpoint and can be scraped by a Prometheus server for detailed monitoring and alerting.

Key metrics include:

- **Latency (`loadbalancer_node_latency_seconds`)**: Measures the response time of each node.
- **Chainhead (`loadbalancer_node_chainhead`)**: Monitors the current block number for each node.
- **Best Endpoint (`loadbalancer_best_endpoint`)**: Indicates the currently selected node for a network.
- **Request Count and Duration (`loadbalancer_requests_total`, `loadbalancer_request_duration_seconds`)**: Track the number of requests and how long they take to process.
- **Blocks behind (`loadbalancer_node_blocks_behind `)**: Track the number of blocks behind for each endpoint relative to the chain tip.

### 5. Proxying Requests

When a request is made to the load balancer, it forwards the request to the best node based on the above logic. The request is routed via an HTTP reverse proxy to the selected Ethereum node.

- If a node fails during the proxying, the load balancer will try another node, ensuring minimal downtime.
- The balancer maintains a cache of reverse proxies to avoid repeatedly creating new ones for the same endpoint, optimizing performance.

## Example Load Balancing Flow

1. A request comes in for the Ethereum Mainnet.
2. The load balancer checks the status of all configured local nodes.
3. It finds that one local node is behind in blocks, so it skips that node.
4. The balancer then checks the remaining local nodes and finds two nodes with the same chainhead, but one has lower latency.
5. The balancer selects the node with the lower latency and forwards the request to it via an HTTP reverse proxy.
6. Metrics are updated (request count, latency, etc.), and the response is sent back to the client.


# Installation

## Prerequisites
- Go 1.18 or higher
- Prometheus (optional): For monitoring the load balancer and nodes.
- Access to EVM RPC nodes

### Build from source

Clone the repository
```bash
git clone https://github.com/StakeSquid/evm-loadbalancer
```

Enter the directory
```bash
cd evm-loadbalancer
```

```bash
go build -o bin/loadbalancer main.go
```
This will generate the `loadbalancer` executable in the `bin` directory.

## Configuration
The load balancer is configured via a YAML file. A sample configuration file (config.yaml) is provided in the repository. Key configuration parameters include:

### Main Configurations
- `port`: The port on which the load balancer listens for incoming requests.
- `log_level`: Logging verbosity. Options are ERROR, INFO, or DEBUG.
- `log_rate_limit`: Time duration between repeated log messages to prevent excessive logging (e.g., 10s).
- `metrics_port`: Port on which Prometheus metrics are served.

### Network Configuration
For each network you want to monitor and balance across nodes:

- `name`: A unique name for the network.
- `local_nodes`: List of local Ethereum RPC endpoints, each defined by:
    - `rpc_endpoint`: The URL of the Ethereum RPC node.
    - `prometheus_endpoint` (optional): URL to fetch server load metrics from Prometheus, used if load tracking is enabled.
- `monitoring_nodes` (optional): List of external monitoring Ethereum RPC endpoints.
- `fallback_nodes` (optional): List of fallback Ethereum RPC endpoints.
- `load_balance_priority` (optional): A list of load-balancing criteria in order of priority. Options are latency, load, and chainhead.
- `load_period` (optional): Time window in seconds to measure server load.
- `local_poll_interval`: How often to poll local nodes for status (in Go duration format, e.g., 1s, 5m).
- `monitoring_poll_interval` (optional): How often to poll monitoring nodes.
- `network_block_diff`: Max block difference allowed between nodes and the network chainhead.
- `use_load_tracker` (optional): Whether to monitor server load (requires Prometheus or similar setups).
- `rpc_timeout`: Timeout duration for RPC calls.
- `rpc_retries`: Number of retries for RPC requests.
- `switch_to_fallback_enabled` (optional): Whether to switch to fallback nodes when falling behind chainhead.
- `switch_to_fallback_block_threshold` (optional): Number of blocks behind before switching to fallback nodes.
#### Note on Optional Parameters
Parameters marked as optional can be omitted. The load balancer will function without them, using default behaviors.
For example, if `monitoring_nodes` or `fallback_nodes` are not provided, the load balancer will rely solely on the `local_nodes`.
If `load_balance_priority` is not specified, the default behavior is to use the chainhead for node selection.

## Example configuration
```yaml
port: "8080"                   # Port on which the load balancer listens for incoming requests
log_level: "INFO"              # Logging level: DEBUG, INFO, or ERROR
log_rate_limit: "10s"          # Rate limit for logging repeated messages
metrics_port: "9101"           # Port for exposing Prometheus metrics

# Define multiple networks that the load balancer will handle
networks:
  - name: "mainnet"                           # Network name, used in request paths
    local_nodes:                              # List of local node RPC and Prometheus endpoints
      - rpc_endpoint: "http://mainnet-1:8545"
        prometheus_endpoint: "http://mainnet-1:9100/metrics"
      - rpc_endpoint: "http://mainnet-2:8545"
        prometheus_endpoint: "http://mainnet-2:9100/metrics"
      - rpc_endpoint: "http://mainnet-3:8545"
        prometheus_endpoint: "http://mainnet-3:9100/metrics"
    monitoring_nodes:                         # (Optional) List of external monitoring node RPC endpoints
      - rpc_endpoint: "http://monitoring-node-1:9545"
    fallback_nodes:                           # (Optional) List of fallback node RPC endpoints
      - rpc_endpoint: "http://fallback-node-1:9656"
      - rpc_endpoint: "http://fallback-node-2:9657"
    load_balance_priority:                    # (Optional) Priority for load balancing: latency, load, chainhead
      - "latency"
      - "load"
      - "chainhead"
    load_period: 1                            # (Optional) Load tracking period (in seconds)
    local_poll_interval: "0.5s"               # Poll interval for local nodes
    monitoring_poll_interval: "1s"            # (Optional) Poll interval for monitoring nodes
    network_block_diff: 5                     # Max allowed block difference
    use_load_tracker: true                    # (Optional) Enable load tracking
    rpc_timeout: "5s"                         # Timeout for RPC calls
    rpc_retries: 3                            # Number of retries for failed RPC calls
    switch_to_fallback_enabled: true          # (Optional) Enable fallback nodes
    switch_to_fallback_block_threshold: 10    # Blocks behind to trigger fallback

  - name: "ropsten"                           # Another network configuration (e.g., a testnet)
    local_nodes:
      - rpc_endpoint: "http://localhost:8548"
    # monitoring_nodes and fallback_nodes can be omitted
    local_poll_interval: "15s"
    network_block_diff: 10
    rpc_timeout: "10s"
    rpc_retries: 5

```

## Minimal configuration
```yaml
port: "8080"                 
log_level: "INFO"            
log_rate_limit: "10s"          
metrics_port: "9101"
networks:        
  - name: "mainnet"            
    local_nodes:
      - rpc_endpoint: "http://localhost:8548"
      - rpc_endpoint: "http://localhost:9656"
    local_poll_interval: "1s"
    network_block_diff: 10
    rpc_timeout: "10s"
    rpc_retries: 5
```

## Running the Load Balancer
After building the project and setting up the configuration file, you can run the load balancer as follows:

```bash
./loadbalancer --config config.yaml
```
Command-line Options
`--config`: Path to the configuration YAML file (default: `config.yaml`).

## Connecting to the StakeSquid EVM Loadbalancer

The load balancer can route your requests to the appropriate Ethereum network based on the configuration provided in `config.yaml`. You can interact with the load balancer via HTTP or HTTPS requests, and it will forward your request to the best available Ethereum node for the selected network.

### How to Connect

The load balancer listens on a specific port as defined in the `config.yaml` file. Each network is identified by its `name` field in the configuration file, and you will use that network name in the URL path when making requests.

For example, assuming you have configured the load balancer to run on port 8080 and have defined the following networks:

- `mainnet`
- `ropsten`

The loadbalancer exposes the endpoints as such:
```bash
http://localhost:8080/mainnet
or
http://localhost:8080/ropsten
```

### Testing the connection

To make an RPC request, simply use the appropriate URL and include your JSON-RPC payload. Here's an example of querying the latest block number on the Ethereum Mainnet:

```bash
curl -X POST http://localhost:8080/mainnet \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
```

## Metrics
The load balancer exposes Prometheus metrics for monitoring. By default, these are served on `http://localhost:9101/metrics`. Metrics include:

- `loadbalancer_node_latency_seconds`: Latency to the nodes in seconds.
- `loadbalancer_node_chainhead`: Current block number at the node.
- `loadbalancer_requests_total`: Total number of requests handled.
- `loadbalancer_request_duration_seconds`: Histogram of request durations.
- `loadbalancer_best_endpoint`: Tracks the best endpoint chosen for each network.
- `loadbalancer_node_blocks_behind`: Tracks the number of blocks behind for each network.

## Monitoring
To monitor the load balancer and nodes, configure your Prometheus server to scrape the /metrics endpoint:

```yaml
scrape_configs:
  - job_name: 'evm-loadbalancer'
    static_configs:
      - targets: ['localhost:9101']
```

## Logging
Logs can be outputted at different verbosity levels:

- `ERROR`: Only log errors.
- `INFO`: Log key information such as startup and major events.
- `DEBUG`: Verbose logging of all events, including chainhead updates.

## License
This project is licensed under the MIT License. See the LICENSE file for details.
