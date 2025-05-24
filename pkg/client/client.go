package client

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type RPCClient struct {
	client    *ethclient.Client
	rpcClient *rpc.Client
	url       string
}

func NewRPCClient(url string, timeout time.Duration) (*RPCClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	rpcClient, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RPC: %w", err)
	}

	ethClient := ethclient.NewClient(rpcClient)

	return &RPCClient{
		client:    ethClient,
		rpcClient: rpcClient,
		url:       url,
	}, nil
}

func (c *RPCClient) GetBlockNumber(ctx context.Context) (int64, time.Duration, error) {
	start := time.Now()

	blockNumber, err := c.client.BlockNumber(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get block number: %w", err)
	}

	latency := time.Since(start)
	return int64(blockNumber), latency, nil
}

func (c *RPCClient) GetBlockByNumber(ctx context.Context, number *big.Int) (int64, error) {
	block, err := c.client.BlockByNumber(ctx, number)
	if err != nil {
		return 0, fmt.Errorf("failed to get block: %w", err)
	}

	return block.Number().Int64(), nil
}

func (c *RPCClient) Close() {
	if c.rpcClient != nil {
		c.rpcClient.Close()
	}
}

func (c *RPCClient) HealthCheck(ctx context.Context) error {
	var result string
	err := c.rpcClient.CallContext(ctx, &result, "web3_clientVersion")
	return err
}

func (c *RPCClient) GetURL() string {
	return c.url
}
