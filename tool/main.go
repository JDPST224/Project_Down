// Usage:
//
//	go run main.go <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]          (direct mode)
//	go run main.go <URL> <THREADS> <DURATION_SEC> <PROXY_TYPE> [CUSTOM_HOST]  (proxy mode)
//
// Proxy types: http, https, sock4, sock5
// Proxies are loaded from proxies.txt (one ip:port per line)
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]              (direct)\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s <URL> <THREADS> <DURATION_SEC> <PROXY_TYPE> [CUSTOM_HOST]  (proxy)\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "PROXY_TYPE: http, https, sock4, sock5\n")
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
	duration := time.Duration(durSec) * time.Second

	// Determine mode: if 4th arg is a valid proxy type, use proxy mode.
	if len(os.Args) >= 5 {
		proxyType := strings.ToLower(os.Args[4])
		switch proxyType {
		case "http", "https", "sock4", "sock5":
			customHost := ""
			if len(os.Args) > 5 {
				customHost = os.Args[5]
			}
			runProxy(rawURL, threads, duration, proxyType, customHost)
			return
		}
	}

	// Direct mode (4th arg is optional custom host, or not provided).
	customHost := ""
	if len(os.Args) >= 5 {
		customHost = os.Args[4]
	}
	runDirect(rawURL, threads, duration, customHost)
}
