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
- **Requests by IP (`loadbalancer_requests_by_ip_total`)**: Track the number of requests by the source IP address.

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
- `network_block_diff`: Max block difference allowed between nodes and the network chainhead before the selection algorithm ignores the endpoints that are X blocks behind and uses.
- `fallback_block_diff`: Max block difference allowed between fallback nodes and the network chainhead before the selection algorithm ignores the endpoints that are X blocks behind.
- `switch_to_fallback_enabled` (optional): Whether to switch to fallback nodes when falling behind chainhead.
- `switch_to_fallback_block_threshold` (optional): Number of blocks behind before switching to fallback nodes.
- `use_load_tracker` (optional): Whether to monitor server load (requires Prometheus or similar setups).
- `rpc_timeout`: Timeout duration for RPC calls.
- `rpc_retries`: Number of retries for RPC requests.

#### Note on Optional Parameters
Parameters marked as optional can be omitted. The load balancer will function without them, using default behaviors.
For example, if `monitoring_nodes` or `fallback_nodes` are not provided, the load balancer will rely solely on the `local_nodes`.
If `load_balance_priority` is not specified, the default behavior is to use the chainhead for node selection.

## Example configuration
```yaml
port: "8080"                   # Port on which the load balancer listens for incoming requests
log_level: "INFO"              # Logging level: TRACE, DEBUG, INFO, or ERROR
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
    load_period: 1                            # (Optional) Load tracking period (in seconds)
    local_poll_interval: "0.5s"               # Poll interval for local nodes
    monitoring_poll_interval: "1s"            # (Optional) Poll interval for monitoring nodes
    network_block_diff: 5                     # Max allowed block difference for a any local nodes to be included in the selection algorithm
    fallback_block_diff: 50                   # Max allowed block difference for a fallback node to be included in the selection algorithm
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

## Advanced configuration

### Network_block_diff parameter
The `network_block_diff` parameter defines the maximum acceptable difference in block heights between local nodes and the highest chainhead observed across all nodes in the network. This setting is crucial in determining whether a local node is considered "in sync" with the overall network.

#### How `network_block_diff` works:
1. **Identifies Nodes Within the "Acceptable Sync Range"**: When selecting an endpoint, the load balancer looks at all available nodes (local, fallback, monitoring) and calculates the difference in block height between each node's chainhead and the highest observed chainhead across all nodes. A node is only considered as a candidate if its block height is within `network_block_diff` blocks of the highest observed chainhead.
2. **Improves Endpoint Selection Stability:** By setting a `network_block_diff`, you can ensure that local nodes slightly behind the highest chainhead are still considered for selection, which is useful in networks where minor block height discrepancies are common. This setting prevents frequent switches between endpoints due to minor differences in block height, enhancing load balancer stability.
3. **Fallback Conditions:** If the local nodes are outside this `network_block_diff` threshold, the load balancer will attempt to switch to a fallback node, provided the fallback node is closer to the highest chainhead or within the allowable difference defined by the fallback_block_diff parameter.

**Example Usage**
If `network_block_diff` is set to 1, then local nodes lagging more than 1 block behind the highest chainhead are excluded from the selection process.
If set to a higher value, say 3, then local nodes that are up to 3 blocks behind the highest observed chainhead are considered for selection, which can be helpful in networks with slight block propagation delays.

### Fallback_block_diff parameter

The `fallback_block_diff` parameter controls the maximum allowable block height difference between any fallback node and the highest observed chainhead across all nodes. It determines when a fallback node is considered too far behind the network chainhead to be used as a reliable endpoint.

#### How `fallback_block_diff` works:
1. **Fallback Node Eligibility:** When the load balancer needs to select a fallback node (usually because local nodes are down or significantly lagging), it assesses each fallback node's block height against the highest chainhead observed across all nodes (local, fallback, and monitoring). Only fallback nodes with block heights within the `fallback_block_diff` of this highest chainhead are considered eligible.
2. **Fallback Node Selection Among Multiple Options:** If multiple fallback nodes are available, the load balancer chooses the one with the highest chainhead, as long as it is within `fallback_block_diff` blocks of the network chainhead.
3. **Fallback Node Exclusion Beyond fallback_block_diff**: If all fallback nodes are lagging behind the network chainhead by more than `fallback_block_diff`, they are excluded from the selection, and the load balancer will not route traffic to any fallback nodes. This may result in an error if no other nodes are available within the acceptable range.

**Example Usage**
If `fallback_block_diff` is set to 10, fallback nodes that are more than 10 blocks behind the highest observed chainhead are excluded from consideration.
Setting `fallback_block_diff` to a higher value (e.g., 20) allows the load balancer to use fallback nodes that are slightly out of sync with the network, which can be beneficial in environments where fallback nodes typically lag due to slower network updates.

### Switch_to_fallback_block_threshold parameter
The `switch_to_fallback_block_threshold` parameter defines a block height threshold that triggers a switch from local nodes to fallback nodes when local nodes fall too far behind the network's chainhead. It is used to ensure that traffic is only routed through local nodes if they are reasonably up-to-date with the rest of the network.

##### How `switch_to_fallback_block_threshold` works:
1. **Switch Condition for Local Nodes:** During endpoint selection, the load balancer checks if the local nodes are lagging behind the highest observed chainhead by more than the `switch_to_fallback_block_threshold` value. If they are, the load balancer disregards the local nodes and attempts to select from available fallback nodes.
2. **Fallback Nodes as Primary:** *When local nodes exceed this threshold, fallback nodes are prioritized for traffic routing, assuming they are within the acceptable range set by `fallback_block_diff`.
3. **Resuming Local Node Priority:** If local nodes later catch up to within the threshold, the load balancer can resume routing traffic through them if they are otherwise eligible (based on network_block_diff and load-balancing priorities).

**Example Usage**
If `switch_to_fallback_block_threshold` is set to 10, the load balancer will switch to fallback nodes if all local nodes are 10 or more blocks behind the network’s chainhead.
If set to 50, the system allows local nodes to lag slightly further behind before falling back, which can be useful in situations where local nodes are generally stable but occasionally experience minor sync delays.

### To recap
The difference between `network_block_diff` and `switch_to_fallback_block_threshold` lies in their purposes and the specific conditions under which they influence endpoint selection:

1. Purpose and Scope:
  - `network_block_diff`: This parameter is a general tolerance level applied only to local nodes to determine if they are "in sync" with the network's chainhead. It controls the maximum allowable block lag for any local node to be considered valid for routing traffic.
  - `switch_to_fallback_block_threshold`: This parameter specifically applies to local nodes and determines when to stop using them and switch to fallback nodes due to lag. It sets the maximum block height difference between local nodes and the network’s highest chainhead that the load balancer will tolerate before routing traffic to fallback nodes.
2. When Each Parameter Is Used:
  - `network_block_diff`: During the endpoint selection process, this parameter ensures that only nodes within a certain block height of the highest chainhead are eligible for traffic. It filters out nodes that are considered too out-of-sync.
  - `switch_to_fallback_block_threshold`: This parameter is checked only when local nodes fall behind the network chainhead. If local nodes exceed this threshold, the load balancer switches exclusively to fallback nodes, assuming they are within the `fallback_block_diff` range.
3. Differences in Handling Local Nodes:
  - `network_block_diff`: Applies to local nodes, fallback nodes, and monitoring nodes equally. A node within network_block_diff blocks of the highest observed chainhead is considered for selection, without any preference or forced switch.
  - `switch_to_fallback_block_threshold`: Only applies when all local nodes exceed this threshold, forcing a switch to fallback nodes. This ensures that local nodes are only used if they’re reasonably close to the network chainhead.

**Example Scenario:**
- `network_block_diff` is set to 5.
- `switch_to_fallback_block_threshold` is set to 10.
- If a local node is 6 blocks behind the network chainhead, it will not be considered due to `network_block_diff`.
- If all local nodes are 11 blocks behind the chainhead, the load balancer will switch to fallback nodes because they exceed `switch_to_fallback_block_threshold`.
- If a fallback node is within 5 blocks of the chainhead (meeting `network_block_diff`), it becomes the active endpoint for traffic.
In short, `network_block_diff` controls node eligibility based on chainhead sync, while `switch_to_fallback_block_threshold` controls whether to prioritize fallback nodes if local nodes are too far behind.

### What about server load and latency?
- When the priority is set to **load, then latency** (meaning nodes are selected first by load, then by latency if loads are equal), and `network_block_diff` is set to 10, the load balancer’s behavior will incorporate both the chainhead block difference and the load and latency of nodes. Here’s how each factor would affect selection in this scenario:

  1. **`network_block_diff` as the First Filter:**
    -  With `network_block_diff` set to 10, the load balancer first filters out any nodes that are more than 10 blocks behind the highest chainhead.
    - This filtering is applied before considering load or latency, so only nodes within the 10-block range of the highest observed chainhead are eligible for selection.
  2. **Applying Load Priority:**
    - Once nodes are filtered to those within the acceptable `network_block_diff`, the load balancer will prioritize nodes based on load.
    - Among the eligible nodes, the one with the lowest load will be prioritized.
    - If there are multiple nodes with similar loads, then latency becomes the next criterion.
  3. **Latency as Secondary Criterion:**
  - If two or more nodes have the same (or very close) load values, the load balancer will then select the node with the **lowest latency** among them.
  - Latency only comes into play once nodes are both within the `network_block_diff` range and have comparable loads.

- When the priority is set to **latency, then load**, and `network_block_diff` is set to 10, the load balancer's selection process still begins by filtering nodes based on `network_block_diff`. However, the choice among eligible nodes will favor those with the lowest latency first, only considering load if multiple nodes have similar latencies. Here’s how it works step-by-step:

  1. **Initial Filtering by `network_block_diff`:**
    - Nodes that are more than 10 blocks behind the highest observed chainhead are filtered out, as set by `network_block_diff`.
    - This ensures that only nodes within a reasonable sync range are considered, regardless of their latency or load values.
  2. **Prioritization by Latency:**
    - Among the remaining eligible nodes, the load balancer will prioritize latency as the primary criterion.
    - The node with the lowest latency will be selected first, assuming it’s within `network_block_diff`.
  3. **Load as a Secondary Criterion:**
  - If two or more nodes have comparable latencies, the load balancer will then choose the node with the lower load among them as a secondary criterion.
  - Load is only considered if latency alone is not sufficient to differentiate among nodes within the allowed block height range.

### Timeouts and retries
The `rpc_timeout` and `rpc_retries` parameters control how the load balancer interacts with each node’s RPC endpoint, defining how long it waits for an RPC response and how many times it retries if an RPC call fails.

#### `rpc_timeout`
The `rpc_timeout` parameter specifies the maximum amount of time (duration) the load balancer will wait for a response from an RPC endpoint before considering the attempt to be timed out.
- **Usage**: When the load balancer makes an RPC request to a node, it will wait for a response for up to the duration specified by `rpc_timeout`.
- **If Exceeded**: If the RPC call doesn’t receive a response within this duration, the request is considered failed, and the load balancer will either retry (if `rpc_retries` is greater than 0) or log the failure and move to the next eligible node.

#### `rpc_retries`
The `rpc_retries` parameter specifies the number of times the load balancer will retry the RPC request if the initial attempt fails (either due to timeout or an error).
- **Usage**: After an RPC call fails, the load balancer will retry the call up to `rpc_retries` times. Each retry is subject to the same `rpc_timeout`.
- **Retries and Backoff**: Often, a short backoff (e.g., a few hundred milliseconds) is applied between retries to allow the RPC endpoint time to recover before the next attempt.
- **After Max Retries**: If the load balancer exhausts all retry attempts without a successful response, it considers the node unreachable or non-responsive for that call, increments error metrics, and logs the issue if configured.


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
- `loadbalancer_requests_by_ip_total`: Total number of requests by source IP

## Monitoring
To monitor the load balancer and nodes, configure your Prometheus server to scrape the /metrics endpoint:

```yaml
scrape_configs:
  - job_name: 'evm-loadbalancer'
    scrape_interval: 15s
    metrics_path: /metrics
    scheme: http
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
