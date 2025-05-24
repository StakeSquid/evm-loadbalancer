package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/evm-loadbalancer/internal/logger"
	"github.com/evm-loadbalancer/internal/types"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type ProxyManager struct {
	httpProxies map[string]*httputil.ReverseProxy
	wsUpgrader  websocket.Upgrader
	networks    map[string]*types.NetworkStatus
	logger      *logrus.Entry
	rateLimiter *logger.RateLimiter
	mu          sync.RWMutex
}

func NewProxyManager(networks map[string]*types.NetworkStatus, logger *logrus.Entry, rateLimiter *logger.RateLimiter) *ProxyManager {
	return &ProxyManager{
		httpProxies: make(map[string]*httputil.ReverseProxy),
		wsUpgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			HandshakeTimeout: 10 * time.Second,
		},
		networks:    networks,
		logger:      logger,
		rateLimiter: rateLimiter,
	}
}

func (p *ProxyManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	networkName, remainingPath := p.extractNetworkFromPath(r.URL.Path)
	if networkName == "" {
		p.writeJSONRPCError(w, -32600, "Network not specified", nil)
		return
	}

	network, exists := p.networks[networkName]
	if !exists {
		p.writeJSONRPCError(w, -32600, "Unknown network", nil)
		return
	}

	if websocket.IsWebSocketUpgrade(r) {
		p.handleWebSocket(w, r, network, remainingPath)
		return
	}

	p.handleHTTP(w, r, network, remainingPath, start)
}

func (p *ProxyManager) handleHTTP(w http.ResponseWriter, r *http.Request, network *types.NetworkStatus, path string, start time.Time) {
	endpoint := network.GetBestEndpoint(types.ProtocolHTTP)
	if endpoint == nil {
		p.rateLimiter.LogError(network.Name, "no_endpoint", "No available endpoint")
		p.writeJSONRPCError(w, -32603, "No available endpoint", nil)
		return
	}

	proxy := p.getOrCreateHTTPProxy(endpoint.URL)

	r.URL.Path = path
	r.URL.Host = endpoint.URL.Host
	r.URL.Scheme = endpoint.URL.Scheme
	r.Host = endpoint.URL.Host

	p.logger.WithFields(logrus.Fields{
		"network":   network.Name,
		"endpoint":  endpoint.URL.String(),
		"method":    r.Method,
		"path":      path,
		"client_ip": getClientIP(r),
	}).Debug("Proxying HTTP request")

	proxy.ServeHTTP(w, r)
}

func (p *ProxyManager) handleWebSocket(w http.ResponseWriter, r *http.Request, network *types.NetworkStatus, path string) {
	endpoint := network.GetBestEndpoint(types.ProtocolWebSocket)
	if endpoint == nil {
		p.rateLimiter.LogError(network.Name, "no_ws_endpoint", "No available WebSocket endpoint")
		p.writeJSONRPCError(w, -32603, "No available WebSocket endpoint", nil)
		return
	}

	targetURL := *endpoint.URL
	targetURL.Path = path

	if targetURL.Scheme == "http" {
		targetURL.Scheme = "ws"
	} else if targetURL.Scheme == "https" {
		targetURL.Scheme = "wss"
	}

	p.logger.WithFields(logrus.Fields{
		"network":   network.Name,
		"endpoint":  targetURL.String(),
		"client_ip": getClientIP(r),
	}).Debug("Proxying WebSocket connection")

	if err := p.proxyWebSocket(w, r, &targetURL); err != nil {
		p.rateLimiter.LogError(network.Name, "ws_proxy_error",
			fmt.Sprintf("WebSocket proxy error: %v", err))
	}
}

func (p *ProxyManager) proxyWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL) error {
	clientConn, err := p.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("failed to upgrade client connection: %w", err)
	}
	defer clientConn.Close()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	targetHeaders := http.Header{}
	for k, v := range r.Header {
		if k != "Upgrade" && k != "Connection" && k != "Sec-Websocket-Key" &&
			k != "Sec-Websocket-Version" && k != "Sec-Websocket-Extensions" {
			targetHeaders[k] = v
		}
	}

	targetConn, _, err := dialer.Dial(target.String(), targetHeaders)
	if err != nil {
		return fmt.Errorf("failed to connect to target: %w", err)
	}
	defer targetConn.Close()

	errChan := make(chan error, 2)

	go func() {
		errChan <- p.copyWebSocketMessages(targetConn, clientConn, "target->client")
	}()

	go func() {
		errChan <- p.copyWebSocketMessages(clientConn, targetConn, "client->target")
	}()

	err = <-errChan
	return err
}

func (p *ProxyManager) copyWebSocketMessages(dst, src *websocket.Conn, direction string) error {
	for {
		messageType, data, err := src.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}

		if err := dst.WriteMessage(messageType, data); err != nil {
			return err
		}
	}
}

func (p *ProxyManager) getOrCreateHTTPProxy(target *url.URL) *httputil.ReverseProxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := target.String()
	if proxy, exists := p.httpProxies[key]; exists {
		return proxy
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 500 {
			p.rateLimiter.LogError(target.String(), "upstream_error",
				fmt.Sprintf("Upstream returned %d", resp.StatusCode))

			// Replace the error response with a JSON-RPC error
			errorResp := map[string]interface{}{
				"jsonrpc": "2.0",
				"error": map[string]interface{}{
					"code":    -32603,
					"message": fmt.Sprintf("Upstream server error: %d", resp.StatusCode),
				},
				"id": 1,
			}

			body, _ := json.Marshal(errorResp)
			resp.Body = ioutil.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Type", "application/json")
			resp.StatusCode = http.StatusOK
		}
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.rateLimiter.LogError(target.String(), "proxy_error",
			fmt.Sprintf("HTTP proxy error: %v", err))
		p.writeJSONRPCError(w, -32603, "Internal proxy error", nil)
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	proxy.Transport = transport

	p.httpProxies[key] = proxy
	return proxy
}

func (p *ProxyManager) extractNetworkFromPath(path string) (string, string) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) == 0 {
		return "", ""
	}

	networkName := parts[0]
	remainingPath := "/"
	if len(parts) > 1 {
		remainingPath = "/" + parts[1]
	}

	return networkName, remainingPath
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		parts := strings.Split(ip, ",")
		return strings.TrimSpace(parts[0])
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

type WebSocketProxy struct {
	clientConn *websocket.Conn
	targetConn *websocket.Conn
	logger     *logrus.Entry
}

func (w *WebSocketProxy) Start() error {
	errChan := make(chan error, 2)

	go func() {
		for {
			messageType, p, err := w.clientConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			if err := w.targetConn.WriteMessage(messageType, p); err != nil {
				errChan <- err
				return
			}
		}
	}()

	go func() {
		for {
			messageType, p, err := w.targetConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			if err := w.clientConn.WriteMessage(messageType, p); err != nil {
				errChan <- err
				return
			}
		}
	}()

	return <-errChan
}

func (p *ProxyManager) writeJSONRPCError(w http.ResponseWriter, code int, message string, id interface{}) {
	if id == nil {
		id = 1
	}

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
		"id": id,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
