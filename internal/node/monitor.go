package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/evm-loadbalancer/internal/logger"
	"github.com/evm-loadbalancer/internal/types"
	"github.com/evm-loadbalancer/pkg/client"
	"github.com/sirupsen/logrus"
)

type Monitor struct {
	node         *types.NodeStatus
	pollInterval time.Duration
	timeout      time.Duration
	retryCount   int
	logger       *logrus.Entry
	rateLimiter  *logger.RateLimiter
}

func NewMonitor(node *types.NodeStatus, pollInterval, timeout time.Duration, retryCount int, logger *logrus.Entry, rateLimiter *logger.RateLimiter) *Monitor {
	return &Monitor{
		node:         node,
		pollInterval: pollInterval,
		timeout:      timeout,
		retryCount:   retryCount,
		logger:       logger.WithField("node", node.URL.String()),
		rateLimiter:  rateLimiter,
	}
}

func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	m.poll(ctx)

	for {
		select {
		case <-ticker.C:
			m.poll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var chainHead int64
	var latency time.Duration
	var err error

	for retry := 0; retry <= m.retryCount; retry++ {
		chainHead, latency, err = m.fetchChainHead(ctx)
		if err == nil {
			break
		}

		if retry < m.retryCount {
			time.Sleep(time.Second * time.Duration(retry+1))
		}
	}

	var load float64
	if m.node.URL.String() != "" && err == nil {
		load = m.fetchLoad(ctx)
	}

	m.node.Update(chainHead, latency, load, err)

	if err != nil {
		m.rateLimiter.LogError(m.node.URL.String(), "poll_error",
			fmt.Sprintf("Failed to poll node: %v", err))
	} else {
		m.logger.WithFields(logrus.Fields{
			"chainhead": chainHead,
			"latency":   latency,
			"load":      load,
		}).Debug("Node poll successful")
	}
}

func (m *Monitor) fetchChainHead(ctx context.Context) (int64, time.Duration, error) {
	rpcClient, err := client.NewRPCClient(m.node.URL.String(), m.timeout)
	if err != nil {
		return 0, 0, err
	}
	defer rpcClient.Close()

	return rpcClient.GetBlockNumber(ctx)
}

func (m *Monitor) fetchLoad(ctx context.Context) float64 {
	promURL := fmt.Sprintf("%s/metrics", m.node.URL.Host)

	req, err := http.NewRequestWithContext(ctx, "GET", promURL, nil)
	if err != nil {
		return 0
	}

	client := &http.Client{Timeout: m.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0
	}

	var metrics struct {
		Load1Min float64 `json:"load_1min"`
	}

	if err := json.Unmarshal(body, &metrics); err != nil {
		return 0
	}

	return metrics.Load1Min
}
