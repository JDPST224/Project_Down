// Agent.go
//
// A single‐file implementation of an Agent that:
// 1. Discovers its public IP and uses it as agentID.
// 2. Connects to the Control Server via SSE to receive “command-enqueued” events in real time.
// 3. Executes a local “l7” binary with provided arguments (URL, threads, timer, custom host).
// 4. Sends periodic heartbeats (every 2s) to update its status (“Ready”, “Sending”, etc.).
// 5. Kills any running “l7” process when a new command arrives or when instructed to stop.
// 6. Cleans up child processes (avoids zombies) and handles graceful shutdown on SIGINT/SIGTERM.
// 7. Uses only Go’s standard library (no third‐party packages).
//
// To build and run:
//   go build -o agent Agent.go
//   ./agent -server http://localhost:8080
//
// The default Control Server URL is http://localhost:8080. You can override via the -server flag.

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
        ForceAttemptHTTP2: false, // <— do not attempt HTTP/2
    }

    // httpClient for short‐lived requests (agent‐status, getPublicIP)
    httpClient = &http.Client{
        Timeout:   10 * time.Second,
        Transport: noHTTP2Transport,
    }

    // sseClient for the SSE “long‐poll” /events; also force HTTP/1.1
    sseClient = &http.Client{
        Transport: noHTTP2Transport,
        // Note: no Timeout here, so reads can block indefinitely
    }
)

func init() {
	// Nothing extra needed here, since we already created httpClient and sseClient globally.
}

// reportStatus POSTs current status to the Control Server at /agent-status.
func reportStatus(controlURL, agentID string) {
	mu.Lock()
	payload := map[string]string{
		"agentID": agentID,
		"status":  status,
	}
	mu.Unlock()

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
	// discard response body
	ioutil.Discard.Write([]byte{})
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
func listenForCommands(controlURL, agentID string) {
	sseURL := fmt.Sprintf("%s/events?agentID=%s", controlURL, agentID)

	for {
		// Use sseClient (no timeout) so this GET can stay open indefinitely
		resp, err := sseClient.Get(sseURL)
		if err != nil {
			log.Printf("[AGENT] SSE connect failed: %v. Retrying in 5s...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("[AGENT] SSE responded with status %d. Retrying in 5s...\n", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("[AGENT] SSE read error: %v. Reconnecting...\n", err)
				resp.Body.Close()
				break
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				eventName := strings.TrimPrefix(line, "event: ")
				if eventName == "command-enqueued" {
					// Next non-empty line should be “data: { ... }”
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
			// Other SSE control lines (comments, keep-alive pings) are ignored
		}
		// On error, loop to reconnect
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

	// 1) Initial status = “Ready”
	reportStatus(controlURL, agentID)

	// 2) Start SSE listener for commands
	go listenForCommands(controlURL, agentID)

	// 3) Periodic heartbeat every 2 seconds
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reportStatus(controlURL, agentID)
		}
	}()

	// 4) Block forever
	select {}
}
