package main

import (
	"bufio"
	"crypto/rand"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dialTimeout = 5 * time.Second
)

var (
	// plaintext proxy sources
	extSources = []string{
		"https://www.proxy-list.download/api/v1/get?type=https",
		"https://api.openproxylist.xyz/https.txt",
		"https://openproxylist.xyz/https.txt",
		"https://proxyspace.pro/https.txt",
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=https&timeout=1000&country=all&ssl=all&anonymity=all",
	}
	// HTML proxy sources
	htmlSources = []string{
		"https://free-proxy-list.net/",
		"https://www.sslproxies.org/",
		"https://www.proxy-list.download/HTTPS",
		"https://www.us-proxy.org/",
	}
	proxyRegex = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d{1,5}\b`)
	userAgents = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		"Mozilla/5.0 (X11; Linux x86_64)",
	}
)

// ProxyResult holds details of a proxy test
type ProxyResult struct {
	Addr      string
	Latency   time.Duration
	Anonymous bool
}

var (
	// counters
	textCount     int64
	htmlCount     int64
	evaluateCount int64
	// live store
	liveProxies = make(map[string]struct{})
	liveList    []string
	mu          sync.RWMutex
)

func main() {
	// flags
	topN := flag.Int("top", 100, "number of high-quality to keep (unused)")
	workers := flag.Int("workers", 200, "concurrency level")
	flag.Parse()

	// start server immediately
	go startServer()

	// scrape
	raw := scrapeAll()
	unique := dedupe(raw)
	log.Printf("📝 Text: %d   🌐 HTML: %d   🔑 Unique: %d",
		atomic.LoadInt64(&textCount), atomic.LoadInt64(&htmlCount), len(unique))

	// evaluate (adds successful into liveList)
	log.Println("🚧 Evaluating proxies...")
	results := evaluateProxies(unique, *workers)
	log.Printf("✅ Tested %d, %d succeeded.", atomic.LoadInt64(&evaluateCount), len(results))

	// optionally select high-quality for metrics
	sort.Slice(results, func(i, j int) bool { return results[i].Latency < results[j].Latency })
	hqCount := min(len(results), *topN)
	log.Printf("🔝 Top %d high-quality selected.", hqCount)

	// block
	select {}
}

func startServer() {
	http.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		mu.RLock()
		defer mu.RUnlock()
		for _, addr := range liveList {
			fmt.Fprintln(w, addr)
		}
	})
	log.Println("🚀 Server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func scrapeAll() []string {
	var wg sync.WaitGroup
	ch := make(chan string, 2000)
	for _, src := range extSources {
		wg.Add(1)
		go func(u string) { defer wg.Done(); fetchText(u, ch) }(src)
	}
	for _, src := range htmlSources {
		wg.Add(1)
		go func(u string) { defer wg.Done(); fetchHTML(u, ch) }(src)
	}
	go func() { wg.Wait(); close(ch) }()

	var all []string
	for addr := range ch {
		all = append(all, addr)
	}
	return all
}

func fetchText(src string, out chan<- string) {
	client := newClient()
	resp, err := client.Get(src)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	
	s := bufio.NewScanner(resp.Body)
	for s.Scan() {
		addr := s.Text()
		if isProxyFormat(addr) {
			atomic.AddInt64(&textCount, 1)
			out <- addr
		}
	}
}

func fetchHTML(src string, out chan<- string) {
	client := newClient()
	resp, err := client.Get(src)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	for _, addr := range proxyRegex.FindAllString(string(body), -1) {
		if isProxyFormat(addr) {
			atomic.AddInt64(&htmlCount, 1)
			out <- addr
		}
	}
}

func newClient() *http.Client {
	ru := userAgents[randomInt(len(userAgents))]
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext}
	iClient := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	iClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Set("User-Agent", ru)
		return nil
	}
	return iClient
}

func evaluateProxies(list []string, workers int) []ProxyResult {
	in := make(chan string, len(list))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for addr := range in {
				atomic.AddInt64(&evaluateCount, 1)
				if pr, ok := testProxy(addr); ok {
					mu.Lock()
					addLive(pr.Addr)
					mu.Unlock()
					// optionally store pr for HQ metrics
				}
			}
		}()
	}
	for _, a := range list { in <- a }
	close(in)
	wg.Wait()

	var res []ProxyResult
	mu.RLock()
	for _, addr := range liveList {
		res = append(res, ProxyResult{Addr: addr})
	}
	mu.RUnlock()
	return res
}

func testProxy(addr string) (ProxyResult, bool) {
	proxyURL := &url.URL{Scheme: "http", Host: addr}
	iClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext}}

	start := time.Now()
	resp, err := iClient.Get("https://httpbin.org/headers")
	if err != nil {
		return ProxyResult{}, false
	}
	lat := time.Since(start)
	body, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	anon := !regexp.MustCompile(`"X-Forwarded-For":`).Match(body)
	return ProxyResult{Addr: addr, Latency: lat, Anonymous: anon}, true
}

func dedupe(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	var u []string
	for _, a := range list {
		if _, ok := seen[a]; !ok {
			seen[a] = struct{}{}
			u = append(u, a)
		}
	}
	return u
}

func isProxyFormat(p string) bool {
	h, po, err := net.SplitHostPort(p)
	return err == nil && net.ParseIP(h) != nil && po != ""
}

func addLive(addr string) {
	if _, exists := liveProxies[addr]; !exists {
		liveProxies[addr] = struct{}{}
		liveList = append(liveList, addr)
	}
}

func randomInt(n int) int {
	b, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(b.Int64())
}

func min(a, b int) int { if a < b { return a }; return b }
