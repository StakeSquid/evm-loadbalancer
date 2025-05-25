package selector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/evm-loadbalancer/internal/logger"
	"github.com/evm-loadbalancer/internal/metrics"
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
	metrics            *metrics.Collector
}

func NewEndpointSelector(
	networkStatus *types.NetworkStatus,
	strategies []types.LoadBalancingStrategy,
	blockDiffThreshold int64,
	selectionInterval time.Duration,
	logger *logrus.Entry,
	rateLimiter *logger.RateLimiter,
	metrics *metrics.Collector,
) *EndpointSelector {
	return &EndpointSelector{
		networkStatus:      networkStatus,
		strategies:         strategies,
		blockDiffThreshold: blockDiffThreshold,
		selectionInterval:  selectionInterval,
		logger:             logger.WithField("network", networkStatus.Name),
		rateLimiter:        rateLimiter,
		metrics:            metrics,
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
	startTime := time.Now()
	s.logger.Debug("Starting endpoint selection")

	networkChainHead := s.calculateNetworkChainHead()
	s.networkStatus.SetNetworkChainHead(networkChainHead)
	s.logger.WithField("network_chainhead", networkChainHead).Debug("Calculated network chain head")

	if s.metrics != nil {
		s.metrics.UpdateNetworkChainHead(s.networkStatus.Name, networkChainHead)
	}

	s.updateBlocksBehind(networkChainHead)

	previousHTTP := s.networkStatus.GetBestEndpoint(types.ProtocolHTTP)
	previousWS := s.networkStatus.GetBestEndpoint(types.ProtocolWebSocket)

	httpEndpoint := s.selectEndpointForProtocol(types.ProtocolHTTP, networkChainHead)
	wsEndpoint := s.selectEndpointForProtocol(types.ProtocolWebSocket, networkChainHead)

	s.networkStatus.SetBestEndpoint(httpEndpoint, types.ProtocolHTTP)
	s.networkStatus.SetBestEndpoint(wsEndpoint, types.ProtocolWebSocket)

	// Track endpoint changes
	if s.metrics != nil {
		if previousHTTP != httpEndpoint && previousHTTP != nil && httpEndpoint != nil {
			s.metrics.RecordSelectionChange(s.networkStatus.Name, string(types.ProtocolHTTP))
		}
		if previousWS != wsEndpoint && previousWS != nil && wsEndpoint != nil {
			s.metrics.RecordSelectionChange(s.networkStatus.Name, string(types.ProtocolWebSocket))
		}

		duration := time.Since(startTime)
		s.metrics.RecordSelectionDuration(s.networkStatus.Name, duration)
	}

	s.logger.WithFields(logrus.Fields{
		"network_chainhead": networkChainHead,
		"http_endpoint":     getEndpointURL(httpEndpoint),
		"ws_endpoint":       getEndpointURL(wsEndpoint),
		"selection_duration": time.Since(startTime).Milliseconds(),
	}).Info("Best endpoints selected")
}

func (s *EndpointSelector) calculateNetworkChainHead() int64 {
	var maxChainHead int64
	var healthyNodeCount int

	allNodes := append(s.networkStatus.LoadBalancingNodes, s.networkStatus.ReferenceNodes...)

	for _, node := range allNodes {
		chainHead, _, _, healthy, _ := node.GetStatus()
		s.logger.WithFields(logrus.Fields{
			"node":       node.URL.String(),
			"chainhead":  chainHead,
			"healthy":    healthy,
			"node_type":  node.NodeType,
		}).Debug("Checking node for network chain head")
		if healthy && chainHead > maxChainHead {
			maxChainHead = chainHead
		}
		if healthy {
			healthyNodeCount++
		}
	}

	s.logger.WithFields(logrus.Fields{
		"max_chainhead":      maxChainHead,
		"healthy_node_count": healthyNodeCount,
		"total_nodes":        len(allNodes),
	}).Debug("Network chain head calculation complete")

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
	s.logger.WithFields(logrus.Fields{
		"protocol":           protocol,
		"network_chainhead": networkChainHead,
	}).Debug("Selecting endpoint for protocol")

	candidates := s.getEligibleNodes(protocol, networkChainHead)

	if len(candidates) == 0 {
		s.rateLimiter.LogError(s.networkStatus.Name, "no_eligible_nodes",
			"No eligible nodes available")
		s.logger.WithField("protocol", protocol).Warn("No eligible nodes found")
		if s.metrics != nil {
			s.metrics.RecordSelectionAttempt(s.networkStatus.Name, string(protocol), "no_candidates")
		}
		return nil
	}

	s.logger.WithFields(logrus.Fields{
		"protocol":         protocol,
		"candidate_count": len(candidates),
	}).Debug("Found eligible candidates")

	if s.metrics != nil {
		s.metrics.RecordSelectionAttempt(s.networkStatus.Name, string(protocol), "success")
	}

	return s.applyStrategies(candidates)
}

func (s *EndpointSelector) getEligibleNodes(protocol types.Protocol, networkChainHead int64) []*types.NodeStatus {
	var candidates []*types.NodeStatus
	var ineligibleReasons = make(map[string]string)
	var eligibleLBCount, eligibleFallbackCount int

	loadBalancingHealthy := false
	for _, node := range s.networkStatus.LoadBalancingNodes {
		if eligible, reason := s.isNodeEligibleWithReason(node, protocol, networkChainHead); eligible {
			candidates = append(candidates, node)
			loadBalancingHealthy = true
			eligibleLBCount++
			s.logger.WithFields(logrus.Fields{
				"node":     node.URL.String(),
				"protocol": protocol,
			}).Debug("Load balancing node is eligible")
		} else {
			ineligibleReasons[node.URL.String()] = reason
			if s.metrics != nil {
				s.metrics.RecordIneligibleNode(s.networkStatus.Name, string(protocol), reason)
			}
		}
	}

	s.logger.WithFields(logrus.Fields{
		"load_balancing_healthy": loadBalancingHealthy,
		"eligible_lb_nodes":      len(candidates),
		"total_lb_nodes":         len(s.networkStatus.LoadBalancingNodes),
	}).Debug("Load balancing nodes evaluation complete")

	if !loadBalancingHealthy {
		s.logger.Debug("No healthy load balancing nodes, checking fallback nodes")
		for _, node := range s.networkStatus.FallbackNodes {
			if eligible, reason := s.isNodeEligibleWithReason(node, protocol, networkChainHead); eligible {
				candidates = append(candidates, node)
				eligibleFallbackCount++
				s.logger.WithFields(logrus.Fields{
					"node":     node.URL.String(),
					"protocol": protocol,
				}).Debug("Fallback node is eligible")
			} else {
				ineligibleReasons[node.URL.String()] = reason
				if s.metrics != nil {
					s.metrics.RecordIneligibleNode(s.networkStatus.Name, string(protocol), reason)
				}
			}
		}
	}

	if s.metrics != nil {
		s.metrics.UpdateEligibleNodes(s.networkStatus.Name, string(protocol), string(types.NodeTypeLoadBalancing), eligibleLBCount)
		s.metrics.UpdateEligibleNodes(s.networkStatus.Name, string(protocol), string(types.NodeTypeFallback), eligibleFallbackCount)
	}

	if len(ineligibleReasons) > 0 {
		s.logger.WithField("ineligible_nodes", ineligibleReasons).Debug("Nodes excluded from selection")
	}

	return candidates
}

func (s *EndpointSelector) isNodeEligible(node *types.NodeStatus, protocol types.Protocol, networkChainHead int64) bool {
	eligible, _ := s.isNodeEligibleWithReason(node, protocol, networkChainHead)
	return eligible
}

func (s *EndpointSelector) isNodeEligibleWithReason(node *types.NodeStatus, protocol types.Protocol, networkChainHead int64) (bool, string) {
	chainHead, latency, load, healthy, blocksBehind := node.GetStatus()

	if !healthy {
		return false, "node unhealthy"
	}

	if !s.isProtocolCompatible(node.Protocol, protocol) {
		return false, fmt.Sprintf("protocol mismatch: node=%s, requested=%s", node.Protocol, protocol)
	}

	if blocksBehind > s.blockDiffThreshold {
		return false, fmt.Sprintf("blocks behind threshold: %d > %d", blocksBehind, s.blockDiffThreshold)
	}

	if chainHead > networkChainHead+s.blockDiffThreshold {
		return false, fmt.Sprintf("chain head too far ahead: %d > %d+%d", chainHead, networkChainHead, s.blockDiffThreshold)
	}

	s.logger.WithFields(logrus.Fields{
		"node":              node.URL.String(),
		"chainhead":         chainHead,
		"blocks_behind":     blocksBehind,
		"latency_ms":        latency.Milliseconds(),
		"load":              load,
		"healthy":           healthy,
		"protocol":          protocol,
		"network_chainhead": networkChainHead,
	}).Debug("Node eligibility check passed")

	return true, ""
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

	// Log initial candidate state
	var candidateInfo []map[string]interface{}
	for _, node := range candidates {
		chainHead, latency, load, _, blocksBehind := node.GetStatus()
		candidateInfo = append(candidateInfo, map[string]interface{}{
			"url":           node.URL.String(),
			"chainhead":     chainHead,
			"latency_ms":    latency.Milliseconds(),
			"load":          load,
			"blocks_behind": blocksBehind,
		})
	}
	s.logger.WithField("candidates_before_sort", candidateInfo).Debug("Applying selection strategies")

	for _, strategy := range s.strategies {
		s.logger.WithField("strategy", strategy).Debug("Applying strategy")
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

	// Log final selection
	selected := candidates[0]
	chainHead, latency, load, _, blocksBehind := selected.GetStatus()
	s.logger.WithFields(logrus.Fields{
		"selected_node":  selected.URL.String(),
		"chainhead":      chainHead,
		"latency_ms":     latency.Milliseconds(),
		"load":           load,
		"blocks_behind":  blocksBehind,
		"total_candidates": len(candidates),
	}).Debug("Node selected after applying strategies")

	return selected
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
