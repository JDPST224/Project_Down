package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command represents a control instruction from the server.
type Command struct {
	Action     string `json:"action"`      // "start", "stop", or "none"
	URL        string `json:"url"`         // target URL for the load test
	Threads    int    `json:"threads"`     // number of threads/connections
	Timer      int    `json:"timer"`       // duration in seconds
	CustomHost string `json:"custom_host"` // optional Host header override
}

var (
	mu             sync.Mutex
	currentCommand *exec.Cmd
	status         = "Ready"

	// Create a Transport that disables HTTP/2
	noHTTP2Transport = &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
		ForceAttemptHTTP2: false, // do not attempt HTTP/2
	}

	// httpClient for short‐lived requests (agent‐status, getPublicIP)
	httpClient = &http.Client{
		Timeout:   10 * time.Second,
		Transport: noHTTP2Transport,
	}

	// sseClient for long‐lived SSE reads without any timeout
	sseClient = &http.Client{
		Transport: noHTTP2Transport,
		// Note: no Timeout here, so reads can block indefinitely
	}
)

// reportStatus POSTs current status to the Control Server at /agent-status.
// We removed any “skip if unchanged” logic—this always POSTs every time it’s called.
func reportStatus(controlURL, agentID string) {
	mu.Lock()
	current := status
	mu.Unlock()

	payload := map[string]string{
		"agentID": agentID,
		"status":  current,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[AGENT] Failed to marshal status payload: %v\n", err)
		return
	}
	endpoint := controlURL + "/agent-status"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		log.Printf("[AGENT] Failed to create status POST request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[AGENT] Failed to POST status: %v\n", err)
		return
	}
	ioutil.Discard.Write([]byte{}) // discard body
	resp.Body.Close()
}

// executeL7 starts the “l7” binary with given parameters.
// It kills any existing “l7” process, updates status, and schedules a stop after timer seconds.
func executeL7(controlURL, agentID, url string, threads, timer int, customHost string) {
	// Kill any existing l7 process first
	mu.Lock()
	if currentCommand != nil && currentCommand.Process != nil {
		_ = currentCommand.Process.Kill()
		go func(cmd *exec.Cmd) { cmd.Wait() }(currentCommand)
		currentCommand = nil
	}
	status = "Sending"
	mu.Unlock()

	// Report “Sending” status
	reportStatus(controlURL, agentID)

	// Build command arguments
	args := []string{url, fmt.Sprintf("%d", threads), fmt.Sprintf("%d", timer)}
	if customHost != "" {
		args = append(args, customHost)
	}

	// Start the l7 binary
	cmd := exec.Command("./l7", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("[AGENT] Failed to start l7: %v\n", err)
		mu.Lock()
		status = "Error"
		mu.Unlock()
		reportStatus(controlURL, agentID)
		return
	}

	// Store the running process
	mu.Lock()
	currentCommand = cmd
	mu.Unlock()

	// After timer seconds, kill the process and report “Ready”
	go func(cmd *exec.Cmd, duration int) {
		time.Sleep(time.Duration(duration) * time.Second)
		mu.Lock()
		if currentCommand != nil && currentCommand.Process != nil {
			_ = currentCommand.Process.Kill()
			go func(c *exec.Cmd) { c.Wait() }(currentCommand)
			currentCommand = nil
		}
		status = "Ready"
		mu.Unlock()
		reportStatus(controlURL, agentID)
	}(cmd, timer)
}

// listenForCommands connects to the Control Server’s SSE endpoint (/events?agentID=...)
// and listens for “command-enqueued” events. When one arrives, it extracts the Command
// and either starts or stops the l7 process accordingly.
//
// We use a short DialContext timeout (10 s) for the initial connection, but once it’s open,
// the read loop has no deadline so it can run indefinitely. If any read error occurs
// (e.g. EOF), we simply reconnect immediately with exponential backoff.
func listenForCommands(controlURL, agentID string) {
	sseURL := fmt.Sprintf("%s/events?agentID=%s", controlURL, agentID)

	backoff := 500 * time.Millisecond
	maxBackoff := 10 * time.Second

	for {
		// Create a custom Transport that only enforces a 10 s dial timeout.
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		transport := &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
			ForceAttemptHTTP2: false,
			DialContext:       dialer.DialContext,
		}
		// Use a one‐off client to establish the connection
		oneOff := &http.Client{Transport: transport}

		req, _ := http.NewRequest(http.MethodGet, sseURL, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "Go-SSE-Agent/1.0")

		resp, err := oneOff.Do(req)
		if err != nil {
			log.Printf("[AGENT] SSE connect failed: %v. Retrying in %v...\n", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("[AGENT] SSE responded with status %d. Retrying in %v...\n", resp.StatusCode, backoff)
			resp.Body.Close()
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Reset backoff on successful connect
		backoff = 500 * time.Millisecond

		// Now read lines indefinitely from resp.Body
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				// This covers unexpected EOF or any network error. Reconnect.
				log.Printf("[AGENT] SSE read error: %v. Reconnecting...\n", err)
				resp.Body.Close()
				break // break out of read loop → reconnect
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				eventName := strings.TrimPrefix(line, "event: ")
				if eventName == "command-enqueued" {
					// Next non-empty “data: { ... }” line
					for {
						dataLine, err2 := reader.ReadString('\n')
						if err2 != nil {
							log.Printf("[AGENT] SSE read error: %v. Reconnecting...\n", err2)
							resp.Body.Close()
							break
						}
						dataLine = strings.TrimSpace(dataLine)
						if dataLine == "" {
							continue
						}
						if strings.HasPrefix(dataLine, "data: ") {
							raw := strings.TrimPrefix(dataLine, "data: ")
							raw = strings.TrimSpace(raw)
							var cmd Command
							if err3 := json.Unmarshal([]byte(raw), &cmd); err3 != nil {
								log.Printf("[AGENT] Failed to unmarshal Command: %v\n", err3)
							} else {
								switch cmd.Action {
								case "start":
									log.Printf("[AGENT] Received START → %+v\n", cmd)
									go executeL7(controlURL, agentID, cmd.URL, cmd.Threads, cmd.Timer, cmd.CustomHost)
								case "stop":
									log.Printf("[AGENT] Received STOP\n")
									mu.Lock()
									if currentCommand != nil && currentCommand.Process != nil {
										_ = currentCommand.Process.Kill()
										go func(c *exec.Cmd) { c.Wait() }(currentCommand)
										currentCommand = nil
									}
									status = "Ready"
									mu.Unlock()
									reportStatus(controlURL, agentID)
								default:
									// Ignore other actions
								}
							}
							break
						}
					}
				}
			}
			// Other SSE lines (comments, keep‐alives) get ignored
		}
		// On any read error, loop to reconnect
	}
}

// getPublicIP fetches the agent’s public IP via api.ipify.org.
func getPublicIP() (string, error) {
	const ipService = "https://api.ipify.org?format=text"
	resp, err := httpClient.Get(ipService)
	if err != nil {
		return "", fmt.Errorf("failed to fetch public IP: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read IP response: %w", err)
	}

	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty IP returned from %s", ipService)
	}
	return ip, nil
}

func main() {
	// Parse flags
	serverFlag := flag.String("server", "http://localhost:8080", "Control Server URL (e.g. https://example.com)")
	flag.Parse()

	// Discover public IP for agentID
	ip, err := getPublicIP()
	if err != nil {
		log.Fatalf("[AGENT] Could not determine public IP: %v\n", err)
	}
	agentID := ip
	controlURL := *serverFlag

	log.Printf("[AGENT] %s starting; connecting to %s\n", agentID, controlURL)

	// Handle graceful shutdown: kill any running l7 process before exit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[AGENT] Shutdown signal received, cleaning up…\n")
		mu.Lock()
		if currentCommand != nil && currentCommand.Process != nil {
			_ = currentCommand.Process.Kill()
			go func(c *exec.Cmd) { c.Wait() }(currentCommand)
			currentCommand = nil
		}
		mu.Unlock()
		os.Exit(0)
	}()

	// 1) Initial status = “Ready” + first heartbeat
	reportStatus(controlURL, agentID)

	// 2) Start SSE listener for commands
	go listenForCommands(controlURL, agentID)

	// 3) Periodic heartbeat every 1 second (UTC)
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reportStatus(controlURL, agentID)
		}
	}()

	// 4) Block forever
	select {}
}
