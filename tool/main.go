// Usage:
//
//	go run main.go [--jitter <ms>] <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]          (direct mode)
//	go run main.go [--jitter <ms>] <URL> <THREADS> <DURATION_SEC> <PROXY_TYPE> [CUSTOM_HOST]  (proxy mode)
//
// Proxy types: http, https, sock4, sock5
// Proxies are loaded from proxies.txt (one ip:port per line)
//
// Flags:
//
//	--jitter <ms>  Maximum per-request sleep in milliseconds to avoid zero-delay bot signatures (default: 0)
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

var jitterMs = flag.Int("jitter", 0, "max per-request jitter in milliseconds (0 = disabled)")

func main() {
	flag.Parse()
	args := flag.Args()

	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [--jitter <ms>] <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]              (direct)\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [--jitter <ms>] <URL> <THREADS> <DURATION_SEC> <PROXY_TYPE> [CUSTOM_HOST]  (proxy)\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "PROXY_TYPE: http, https, sock4, sock5\n")
		fmt.Fprintf(os.Stderr, "FLAGS:\n")
		fmt.Fprintf(os.Stderr, "  --jitter <ms>  max per-request sleep in ms (default 0)\n")
		os.Exit(1)
	}

	threads, err := strconv.Atoi(args[1])
	if err != nil || threads <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid THREADS (%q): must be a positive integer.\n", args[1])
		os.Exit(1)
	}
	durSec, err := strconv.Atoi(args[2])
	if err != nil || durSec <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid DURATION_SEC (%q): must be a positive integer.\n", args[2])
		os.Exit(1)
	}

	rawURL := args[0]
	duration := time.Duration(durSec) * time.Second

	// Determine mode: if 4th arg is a valid proxy type, use proxy mode.
	if len(args) >= 4 {
		proxyType := strings.ToLower(args[3])
		switch proxyType {
		case "http", "https", "sock4", "sock5":
			customHost := ""
			if len(args) > 4 {
				customHost = args[4]
			}
			runProxy(rawURL, threads, duration, proxyType, customHost, *jitterMs)
			return
		}
	}

	// Direct mode (4th arg is optional custom host, or not provided).
	customHost := ""
	if len(args) >= 4 {
		customHost = args[3]
	}
	runDirect(rawURL, threads, duration, customHost, *jitterMs)
}
