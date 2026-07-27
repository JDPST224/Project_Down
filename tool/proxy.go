// Proxy-based stress test.
//
// Usage: go run main.go <URL> <THREADS> <DURATION_SEC> <PROXY_TYPE> [CUSTOM_HOST]
// Proxy types: http, https, sock4, sock5
// Proxies are loaded from proxies.txt (one ip:port per line)
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

// ─── Proxy Manager ────────────────────────────────────────────────────────────

// ProxyManager coordinates proxy usage, worker lifecycle, and metrics.
type ProxyManager struct {
	cfg StressConfig

	proxiesMu sync.Mutex
	proxies   []string

	// Metrics
	totalReqs   atomic.Int64
	totalErrors atomic.Int64
}

func NewProxyManager(cfg StressConfig) *ProxyManager {
	return &ProxyManager{
		cfg: cfg,
	}
}

func (m *ProxyManager) snapshotProxies() []string {
	m.proxiesMu.Lock()
	defer m.proxiesMu.Unlock()
	out := make([]string, len(m.proxies))
	copy(out, m.proxies)
	return out
}

// ─── Entry point ──────────────────────────────────────────────────────────────

func runProxy(rawURL string, threads int, duration time.Duration, proxyType string, customHost string) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Hostname() == "" {
		fmt.Fprintf(os.Stderr, "Invalid URL: %q\n", rawURL)
		os.Exit(1)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		fmt.Fprintf(os.Stderr, "URL scheme must be http or https, got %q\n", parsedURL.Scheme)
		os.Exit(1)
	}

	path := parsedURL.RequestURI()
	if path == "" {
		path = "/"
	}

	// Load proxies from proxies.txt
	proxies, err := loadProxies("proxies.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load proxies.txt: %v\n", err)
		os.Exit(1)
	}
	if len(proxies) == 0 {
		fmt.Fprintf(os.Stderr, "No proxies found in proxies.txt\n")
		os.Exit(1)
	}
	slog.Info("loaded proxies", "count", len(proxies))

	cfg := StressConfig{
		Target:     parsedURL,
		Threads:    threads,
		Duration:   duration,
		CustomHost: customHost,
		Port:       determinePort(parsedURL),
		Path:       path,
		ProxyType:  proxyType,
		Proxies:    proxies,
	}

	mgr := NewProxyManager(cfg)
	mgr.proxies = proxies

	// Root context: cancelled by duration or signal.
	rootCtx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			slog.Info("interrupt received; shutting down early")
			cancel()
		case <-rootCtx.Done():
		}
	}()

	go mgr.runProxyStats(rootCtx, 5*time.Second)

	slog.Info("stress test starting", "url", rawURL, "threads", threads, "duration", cfg.Duration, "proxy_type", proxyType)
	mgr.runProxyManager(rootCtx)
	slog.Info("stress test completed",
		"requests", mgr.totalReqs.Load(),
		"errors", mgr.totalErrors.Load(),
	)
}

// loadProxies reads proxies from a file (one ip:port per line).
func loadProxies(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var proxies []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		proxies = append(proxies, line)
	}
	return proxies, scanner.Err()
}

// ─── Proxy Manager: worker lifecycle ─────────────────────────────────────────

type proxyWorkerEntry struct{ cancel context.CancelFunc }

func (m *ProxyManager) runProxyManager(ctx context.Context) {
	workers := make(map[int][]proxyWorkerEntry) // index-based for simplicity

	spawn := func(idx int) {
		wctx, wcancel := context.WithCancel(ctx)
		workers[idx] = append(workers[idx], proxyWorkerEntry{cancel: wcancel})
		go m.proxyWorkerLoop(wctx, idx)
	}

	// Spawn workers evenly distributed across proxies.
	for i := 0; i < m.cfg.Threads; i++ {
		spawn(i % len(m.cfg.Proxies))
	}

	<-ctx.Done()
	for _, list := range workers {
		for _, w := range list {
			w.cancel()
		}
	}
}

// ─── Proxy Worker ─────────────────────────────────────────────────────────────

func (m *ProxyManager) proxyWorkerLoop(ctx context.Context, proxyIdx int) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	backoff := 50 * time.Millisecond

	// Shared TLS config for the target (used after CONNECT or SOCKS tunnel).
	//nolint:gosec // InsecureSkipVerify is intentional for a stress tool
	targetTLS := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	for {
		if ctx.Err() != nil {
			return
		}

		// Pick a proxy (possibly rotating if the current one fails).
		proxies := m.snapshotProxies()
		if len(proxies) == 0 {
			slog.Warn("no proxies available")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		proxyAddr := proxies[proxyIdx%len(proxies)]

		conn, err := dialViaProxy(ctx, proxyAddr, m.cfg, targetTLS)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, 5*time.Second)
			slog.Debug("proxy dial failed", "proxy", proxyAddr, "err", err, "backoff", backoff)
			m.totalErrors.Add(1)
			// Rotate to next proxy on failure.
			proxyIdx++
			continue
		}
		backoff = 50 * time.Millisecond // reset on successful dial

		method := httpMethods[rng.Intn(len(httpMethods))]

	burstLoop:
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
				alive := m.proxySendBurst(conn, rng, hostHdr, method)
				if alive {
					m.totalReqs.Add(1)
				} else {
					m.totalErrors.Add(1)
					conn.Close()
					proxyIdx++
					break burstLoop
				}
			}
		}
	}
}

// ─── Proxy Dialing ────────────────────────────────────────────────────────────

// dialViaProxy establishes a connection to the target through the specified proxy.
func dialViaProxy(ctx context.Context, proxyAddr string, cfg StressConfig, targetTLS *tls.Config) (net.Conn, error) {
	switch cfg.ProxyType {
	case "http":
		return dialHTTPProxy(ctx, proxyAddr, cfg, targetTLS)
	case "https":
		return dialHTTPSProxy(ctx, proxyAddr, cfg, targetTLS)
	case "sock4":
		return dialSOCKS4(ctx, proxyAddr, cfg, targetTLS)
	case "sock5":
		return dialSOCKS5(ctx, proxyAddr, cfg, targetTLS)
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", cfg.ProxyType)
	}
}

// dialHTTPProxy connects through an HTTP proxy.
// For HTTP targets: sends full URL in request line.
// For HTTPS targets: uses CONNECT tunnel.
func dialHTTPProxy(ctx context.Context, proxyAddr string, cfg StressConfig, targetTLS *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	conn, err := netDialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}

	if cfg.Target.Scheme == "https" {
		// CONNECT tunnel for HTTPS through HTTP proxy.
		hostPort := net.JoinHostPort(cfg.Target.Hostname(), strconv.Itoa(cfg.Port))
		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("CONNECT write: %w", err)
		}

		// Read CONNECT response.
		resp := make([]byte, 1024)
		n, err := conn.Read(resp)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("CONNECT read: %w", err)
		}
		// Check for "200" in response.
		if !bytes.Contains(resp[:n], []byte("200")) {
			conn.Close()
			return nil, fmt.Errorf("CONNECT failed: %s", string(resp[:n]))
		}

		// Upgrade to TLS over the tunnel.
		tlsConn := tls.Client(conn, targetTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		return tlsConn, nil
	}

	// For HTTP targets, return raw connection to proxy.
	// The request will be sent with full URL in the request line.
	return conn, nil
}

// dialHTTPSProxy connects through an HTTPS proxy (proxy over TLS).
// Same as HTTP proxy but the connection to the proxy itself is TLS-encrypted.
func dialHTTPSProxy(ctx context.Context, proxyAddr string, cfg StressConfig, targetTLS *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// Connect to proxy over TLS.
	proxyTLS := &tls.Config{
		InsecureSkipVerify: true,
	}
	tlsConn, err := (&tls.Dialer{NetDialer: netDialer, Config: proxyTLS}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to HTTPS proxy: %w", err)
	}

	if cfg.Target.Scheme == "https" {
		// CONNECT tunnel for HTTPS through HTTPS proxy.
		hostPort := net.JoinHostPort(cfg.Target.Hostname(), strconv.Itoa(cfg.Port))
		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
		if _, err := tlsConn.Write([]byte(req)); err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("CONNECT write: %w", err)
		}

		// Read CONNECT response.
		resp := make([]byte, 1024)
		n, err := tlsConn.Read(resp)
		if err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("CONNECT read: %w", err)
		}
		if !bytes.Contains(resp[:n], []byte("200")) {
			tlsConn.Close()
			return nil, fmt.Errorf("CONNECT failed: %s", string(resp[:n]))
		}

		// Upgrade to TLS over the tunnel (re-handshake for target).
		targetConn := tls.Client(tlsConn, targetTLS)
		if err := targetConn.HandshakeContext(ctx); err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		return targetConn, nil
	}

	// For HTTP targets, return the TLS connection to the proxy.
	return tlsConn, nil
}

// dialSOCKS4 connects through a SOCKS4 proxy.
func dialSOCKS4(ctx context.Context, proxyAddr string, cfg StressConfig, targetTLS *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	conn, err := netDialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to SOCKS4 proxy: %w", err)
	}

	targetHost := cfg.Target.Hostname()
	targetPort := cfg.Port

	// Resolve target IP for SOCKS4 (SOCKS4 doesn't support domain names).
	targetIPs, err := net.LookupIP(targetHost)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("resolve target for SOCKS4: %w", err)
	}
	var targetIP net.IP
	for _, ip := range targetIPs {
		if ip4 := ip.To4(); ip4 != nil {
			targetIP = ip4
			break
		}
	}
	if targetIP == nil {
		conn.Close()
		return nil, fmt.Errorf("no IPv4 address for SOCKS4 target: %s", targetHost)
	}

	// SOCKS4 request: [VN=4][CD=1][DSTPORT][DSTIP][USERID=0x00]
	req := make([]byte, 9)
	req[0] = 0x04 // VN
	req[1] = 0x01 // CD = CONNECT
	binary.BigEndian.PutUint16(req[2:4], uint16(targetPort))
	copy(req[4:8], targetIP.To4())
	req[8] = 0x00 // USERID null terminator

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS4 request write: %w", err)
	}

	// SOCKS4 response: [VN=0][CD][DSTPORT][DSTIP]
	resp := make([]byte, 8)
	if _, err := conn.Read(resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS4 response read: %w", err)
	}
	if resp[1] != 0x5a {
		conn.Close()
		return nil, fmt.Errorf("SOCKS4 request rejected: status=0x%02x", resp[1])
	}

	// If target is HTTPS, upgrade to TLS.
	if cfg.Target.Scheme == "https" {
		tlsConn := tls.Client(conn, targetTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		return tlsConn, nil
	}

	return conn, nil
}

// dialSOCKS5 connects through a SOCKS5 proxy using golang.org/x/net/proxy.
func dialSOCKS5(ctx context.Context, proxyAddr string, cfg StressConfig, targetTLS *tls.Config) (net.Conn, error) {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}

	targetHost := cfg.Target.Hostname()
	targetPort := cfg.Port
	addr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))

	conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 dial: %w", err)
	}

	// If target is HTTPS, upgrade to TLS.
	if cfg.Target.Scheme == "https" {
		tlsConn := tls.Client(conn, targetTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		return tlsConn, nil
	}

	return conn, nil
}

// ─── Request building & sending ───────────────────────────────────────────────

// proxySendBurst sends one HTTP request on conn and drains a small response chunk.
// Returns true if the connection is still usable, false if it should be closed and re-dialled.
func (m *ProxyManager) proxySendBurst(conn net.Conn, rng *rand.Rand, hostHdr, method string) (alive bool) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	var bodyBytes []byte
	buildRequest(buf, m.cfg, rng, method, hostHdr, &bodyBytes)

	// Write header (and optional body) directly from pool buffer — no intermediate copy.
	bufs := net.Buffers{buf.Bytes()}
	if method == "POST" && len(bodyBytes) > 0 {
		bufs = append(bufs, bodyBytes)
	}
	_, writeErr := bufs.WriteTo(conn)
	bufPool.Put(buf) // safe: buf.Bytes() has already been consumed by WriteTo

	if writeErr != nil {
		return false
	}

	// Drain a small chunk to advance the OS receive window.
	// Short deadline so slow servers don't stall the worker.
	conn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
	var tmp [1024]byte
	_, readErr := conn.Read(tmp[:])
	conn.SetReadDeadline(time.Time{})

	// If the read returns a permanent error (not a timeout), the connection is dead.
	if readErr != nil {
		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			// Timeout is expected — the server may not have sent anything yet.
			return true
		}
		// Permanent error (e.g. EOF, RST) — connection is dead.
		return false
	}

	return true
}

// ─── Metrics ──────────────────────────────────────────────────────────────────

func (m *ProxyManager) runProxyStats(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastReqs, lastErrs int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reqs := m.totalReqs.Load()
			errs := m.totalErrors.Load()
			deltaReqs := reqs - lastReqs
			deltaErrs := errs - lastErrs
			lastReqs, lastErrs = reqs, errs
			rps := float64(deltaReqs) / interval.Seconds()
			slog.Info("stats",
				"req/s", fmt.Sprintf("%.0f", rps),
				"errors", deltaErrs,
				"total_reqs", reqs,
				"total_errors", errs,
			)
		}
	}
}
