package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const (
	timeout     = 10 * time.Second
	dialTimeout = 5 * time.Second
	numWorkers  = 200
)

var (
	httpsTextSources = []string{
		"https://www.proxy-list.download/api/v1/get?type=https",
		"https://api.openproxylist.xyz/https.txt",
		"https://openproxylist.xyz/https.txt",
		"https://proxyspace.pro/https.txt",
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=https&timeout=1000&country=all&ssl=all&anonymity=all",
	}
	websiteSources = []string{
		"https://free-proxy-list.net/",
		"https://www.sslproxies.org/",
		"https://www.proxy-list.download/HTTPS",
		"https://www.us-proxy.org/",
	}
	proxyRegex = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d{1,5}\b`)

	validProxies []string
	proxyMu      sync.RWMutex

	textCount int64
	htmlCount int64
)

func main() {
	// 1) scrape & dedupe
	raw := scrapeAll()
	unique := dedupe(raw)

	// 2) log totals only
	log.Printf("📝 Total scraped from text sources: %d", atomic.LoadInt64(&textCount))
	log.Printf("🌐 Total scraped from HTML sources: %d", atomic.LoadInt64(&htmlCount))
	log.Printf("🔑 Total unique candidates: %d", len(unique))

	// 3) start server & check
	go startServer()
	runChecks(unique)

	select {}
}

func scrapeAll() []string {
	var wg sync.WaitGroup
	out := make(chan string, 1000)

	for _, src := range httpsTextSources {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			fetchText(u, out)
		}(src)
	}
	for _, src := range websiteSources {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			fetchHTML(u, out)
		}(src)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	var all []string
	for p := range out {
		all = append(all, p)
	}
	return all
}

func fetchText(src string, out chan<- string) {
	resp, err := http.Get(src)
	if err != nil {
		log.Printf("⚠️ GET %s failed: %v", src, err)
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if isProxyFormat(line) {
			atomic.AddInt64(&textCount, 1)
			out <- line
		}
	}
}

func fetchHTML(src string, out chan<- string) {
	resp, err := http.Get(src)
	if err != nil {
		log.Printf("⚠️ GET %s failed: %v", src, err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("⚠️ read %s error: %v", src, err)
		return
	}

	for _, m := range proxyRegex.FindAllString(string(body), -1) {
		if isProxyFormat(m) {
			atomic.AddInt64(&htmlCount, 1)
			out <- m
		}
	}
}

func dedupe(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	var unique []string
	for _, p := range list {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			unique = append(unique, p)
		}
	}
	return unique
}

func startServer() {
	http.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		proxyMu.RLock()
		defer proxyMu.RUnlock()
		for _, addr := range validProxies {
			fmt.Fprintln(w, addr)
		}
	})
	log.Printf("🚀 Server listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func runChecks(proxies []string) {
	in := make(chan string, len(proxies))
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for addr := range in {
				if checkProxy(addr) {
					proxyMu.Lock()
					if !contains(validProxies, addr) {
						validProxies = append(validProxies, addr)
						log.Printf("✅ live %s", addr)
					}
					proxyMu.Unlock()
				}
			}
		}()
	}

	for _, addr := range proxies {
		in <- addr
	}
	close(in)
	wg.Wait()

	log.Printf("✅ Completed all HTTPS proxy checks (%d live)", len(validProxies))
}

func checkProxy(addr string) bool {
	proxyURL := &url.URL{Scheme: "http", Host: addr}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:       http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		},
	}
	resp, err := client.Get("https://httpbin.org/ip")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func isProxyFormat(proxy string) bool {
	host, port, err := net.SplitHostPort(proxy)
	if err != nil {
		return false
	}
	return net.ParseIP(host) != nil && port != ""
}

func contains(slice []string, item string) bool {
	for _, x := range slice {
		if x == item {
			return true
		}
	}
	return false
}
