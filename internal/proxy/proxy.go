package proxy

import (
	"fmt"
	"io"
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
		http.Error(w, "Network not specified", http.StatusBadRequest)
		return
	}

	network, exists := p.networks[networkName]
	if !exists {
		http.Error(w, "Unknown network", http.StatusNotFound)
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
		http.Error(w, "No available endpoint", http.StatusServiceUnavailable)
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
		http.Error(w, "No available WebSocket endpoint", http.StatusServiceUnavailable)
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
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.rateLimiter.LogError(target.String(), "proxy_error", 
			fmt.Sprintf("HTTP proxy error: %v", err))
		http.Error(w, "Proxy error", http.StatusBadGateway)
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