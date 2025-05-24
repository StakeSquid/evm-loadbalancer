package selector

import (
	"context"
	"sort"
	"time"

	"github.com/evm-loadbalancer/internal/logger"
	"github.com/evm-loadbalancer/internal/types"
	"github.com/sirupsen/logrus"
)

type EndpointSelector struct {
	networkStatus      *types.NetworkStatus
	strategies         []types.LoadBalancingStrategy
	blockDiffThreshold int64
	selectionInterval  time.Duration
	logger             *logrus.Entry
	rateLimiter        *logger.RateLimiter
}

func NewEndpointSelector(
	networkStatus *types.NetworkStatus,
	strategies []types.LoadBalancingStrategy,
	blockDiffThreshold int64,
	selectionInterval time.Duration,
	logger *logrus.Entry,
	rateLimiter *logger.RateLimiter,
) *EndpointSelector {
	return &EndpointSelector{
		networkStatus:      networkStatus,
		strategies:         strategies,
		blockDiffThreshold: blockDiffThreshold,
		selectionInterval:  selectionInterval,
		logger:             logger.WithField("network", networkStatus.Name),
		rateLimiter:        rateLimiter,
	}
}

func (s *EndpointSelector) Start(ctx context.Context) {
	ticker := time.NewTicker(s.selectionInterval)
	defer ticker.Stop()

	s.selectBestEndpoint()

	for {
		select {
		case <-ticker.C:
			s.selectBestEndpoint()
		case <-ctx.Done():
			return
		}
	}
}

func (s *EndpointSelector) selectBestEndpoint() {
	networkChainHead := s.calculateNetworkChainHead()
	s.networkStatus.SetNetworkChainHead(networkChainHead)

	s.updateBlocksBehind(networkChainHead)

	httpEndpoint := s.selectEndpointForProtocol(types.ProtocolHTTP, networkChainHead)
	wsEndpoint := s.selectEndpointForProtocol(types.ProtocolWebSocket, networkChainHead)

	s.networkStatus.SetBestEndpoint(httpEndpoint, types.ProtocolHTTP)
	s.networkStatus.SetBestEndpoint(wsEndpoint, types.ProtocolWebSocket)

	s.logger.WithFields(logrus.Fields{
		"network_chainhead": networkChainHead,
		"http_endpoint":     getEndpointURL(httpEndpoint),
		"ws_endpoint":       getEndpointURL(wsEndpoint),
	}).Info("Best endpoints selected")
}

func (s *EndpointSelector) calculateNetworkChainHead() int64 {
	var maxChainHead int64

	allNodes := append(s.networkStatus.LoadBalancingNodes, s.networkStatus.ReferenceNodes...)

	for _, node := range allNodes {
		chainHead, _, _, healthy, _ := node.GetStatus()
		if healthy && chainHead > maxChainHead {
			maxChainHead = chainHead
		}
	}

	return maxChainHead
}

func (s *EndpointSelector) updateBlocksBehind(networkChainHead int64) {
	allNodes := append(append(s.networkStatus.LoadBalancingNodes,
		s.networkStatus.ReferenceNodes...),
		s.networkStatus.FallbackNodes...)

	for _, node := range allNodes {
		chainHead, _, _, healthy, _ := node.GetStatus()
		if healthy {
			node.SetBlocksBehind(networkChainHead - chainHead)
		}
	}
}

func (s *EndpointSelector) selectEndpointForProtocol(protocol types.Protocol, networkChainHead int64) *types.NodeStatus {
	candidates := s.getEligibleNodes(protocol, networkChainHead)

	if len(candidates) == 0 {
		s.rateLimiter.LogError(s.networkStatus.Name, "no_eligible_nodes",
			"No eligible nodes available")
		return nil
	}

	return s.applyStrategies(candidates)
}

func (s *EndpointSelector) getEligibleNodes(protocol types.Protocol, networkChainHead int64) []*types.NodeStatus {
	var candidates []*types.NodeStatus

	loadBalancingHealthy := false
	for _, node := range s.networkStatus.LoadBalancingNodes {
		if s.isNodeEligible(node, protocol, networkChainHead) {
			candidates = append(candidates, node)
			loadBalancingHealthy = true
		}
	}

	if !loadBalancingHealthy {
		for _, node := range s.networkStatus.FallbackNodes {
			if s.isNodeEligible(node, protocol, networkChainHead) {
				candidates = append(candidates, node)
			}
		}
	}

	return candidates
}

func (s *EndpointSelector) isNodeEligible(node *types.NodeStatus, protocol types.Protocol, networkChainHead int64) bool {
	chainHead, _, _, healthy, blocksBehind := node.GetStatus()

	if !healthy {
		return false
	}

	if !s.isProtocolCompatible(node.Protocol, protocol) {
		return false
	}

	if blocksBehind > s.blockDiffThreshold {
		return false
	}

	if chainHead > networkChainHead+s.blockDiffThreshold {
		return false
	}

	return true
}

func (s *EndpointSelector) isProtocolCompatible(nodeProtocol, requestedProtocol types.Protocol) bool {
	httpProtocols := []types.Protocol{types.ProtocolHTTP, types.ProtocolHTTPS}
	wsProtocols := []types.Protocol{types.ProtocolWebSocket, types.ProtocolWSS}

	if contains(httpProtocols, requestedProtocol) {
		return contains(httpProtocols, nodeProtocol)
	}

	if contains(wsProtocols, requestedProtocol) {
		return contains(wsProtocols, nodeProtocol)
	}

	return false
}

func (s *EndpointSelector) applyStrategies(candidates []*types.NodeStatus) *types.NodeStatus {
	if len(candidates) == 0 {
		return nil
	}

	for _, strategy := range s.strategies {
		switch strategy {
		case types.StrategyChainHead:
			sort.Slice(candidates, func(i, j int) bool {
				chainI, _, _, _, _ := candidates[i].GetStatus()
				chainJ, _, _, _, _ := candidates[j].GetStatus()
				return chainI > chainJ
			})
		case types.StrategyLatency:
			sort.Slice(candidates, func(i, j int) bool {
				_, latencyI, _, _, _ := candidates[i].GetStatus()
				_, latencyJ, _, _, _ := candidates[j].GetStatus()
				return latencyI < latencyJ
			})
		case types.StrategyLoad:
			sort.Slice(candidates, func(i, j int) bool {
				_, _, loadI, _, _ := candidates[i].GetStatus()
				_, _, loadJ, _, _ := candidates[j].GetStatus()
				return loadI < loadJ
			})
		}
	}

	return candidates[0]
}

func contains(slice []types.Protocol, item types.Protocol) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func getEndpointURL(node *types.NodeStatus) string {
	if node == nil {
		return "none"
	}
	return node.URL.String()
}
