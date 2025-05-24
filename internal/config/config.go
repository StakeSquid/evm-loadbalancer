package config

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"time"

	"github.com/evm-loadbalancer/internal/types"
	"gopkg.in/yaml.v3"
)

type Manager struct {
	config *types.Config
}

func NewManager(configPath string) (*Manager, error) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config types.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &Manager{config: &config}, nil
}

func (m *Manager) GetConfig() *types.Config {
	return m.config
}

func (m *Manager) GetNetworkConfigs() []types.NetworkConfig {
	return m.config.Networks
}

func (m *Manager) ParseDuration(d string) (time.Duration, error) {
	return time.ParseDuration(d)
}

func validateConfig(config *types.Config) error {
	if config.Server.Port < 0 {
		return fmt.Errorf("invalid server port: %d", config.Server.Port)
	}

	if config.Server.MetricsPort < 0 {
		return fmt.Errorf("invalid metrics port: %d", config.Server.MetricsPort)
	}

	if len(config.Networks) == 0 {
		return fmt.Errorf("no networks configured")
	}

	for _, network := range config.Networks {
		if network.Name == "" {
			return fmt.Errorf("network name cannot be empty")
		}

		if len(network.LoadBalancingNodes) == 0 && len(network.FallbackNodes) == 0 {
			return fmt.Errorf("network %s must have at least one node", network.Name)
		}

		for _, node := range append(network.LoadBalancingNodes, append(network.ReferenceNodes, network.FallbackNodes...)...) {
			if _, err := url.Parse(node.URL); err != nil {
				return fmt.Errorf("invalid node URL %s: %w", node.URL, err)
			}
		}

		if _, err := time.ParseDuration(network.PollInterval); err != nil {
			return fmt.Errorf("invalid poll interval %s: %w", network.PollInterval, err)
		}

		if _, err := time.ParseDuration(network.Timeout); err != nil {
			return fmt.Errorf("invalid timeout %s: %w", network.Timeout, err)
		}

		if network.BlockDiffThreshold <= 0 {
			return fmt.Errorf("block diff threshold must be positive")
		}
	}

	return nil
}

func (m *Manager) InitializeNetworkStatus() (map[string]*types.NetworkStatus, error) {
	networks := make(map[string]*types.NetworkStatus)

	for _, netConfig := range m.config.Networks {
		status := &types.NetworkStatus{
			Name:               netConfig.Name,
			LoadBalancingNodes: make([]*types.NodeStatus, 0),
			ReferenceNodes:     make([]*types.NodeStatus, 0),
			FallbackNodes:      make([]*types.NodeStatus, 0),
		}

		for _, nodeConfig := range netConfig.LoadBalancingNodes {
			node, err := createNodeStatus(nodeConfig, types.NodeTypeLoadBalancing)
			if err != nil {
				return nil, err
			}
			status.LoadBalancingNodes = append(status.LoadBalancingNodes, node)
		}

		for _, nodeConfig := range netConfig.ReferenceNodes {
			node, err := createNodeStatus(nodeConfig, types.NodeTypeReference)
			if err != nil {
				return nil, err
			}
			status.ReferenceNodes = append(status.ReferenceNodes, node)
		}

		for _, nodeConfig := range netConfig.FallbackNodes {
			node, err := createNodeStatus(nodeConfig, types.NodeTypeFallback)
			if err != nil {
				return nil, err
			}
			status.FallbackNodes = append(status.FallbackNodes, node)
		}

		networks[netConfig.Name] = status
	}

	return networks, nil
}

func createNodeStatus(config types.NodeConfig, nodeType types.NodeType) (*types.NodeStatus, error) {
	parsedURL, err := url.Parse(config.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %s: %w", config.URL, err)
	}

	protocol := types.ProtocolHTTP
	switch parsedURL.Scheme {
	case "https":
		protocol = types.ProtocolHTTPS
	case "ws":
		protocol = types.ProtocolWebSocket
	case "wss":
		protocol = types.ProtocolWSS
	}

	if config.Protocol != "" {
		protocol = config.Protocol
	}

	return &types.NodeStatus{
		URL:      parsedURL,
		NodeType: nodeType,
		Protocol: protocol,
	}, nil
}
