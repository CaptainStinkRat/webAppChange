// Package handler manages HTTP endpoints and SSE streaming for the codegen chat.
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"webAppChange/app/internal/agent"
)

// ── SSE event types ─────────────────────────────────────────────────

type Event struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Code    string `json:"code,omitempty"`
	Review  string `json:"review,omitempty"`
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ── Session ─────────────────────────────────────────────────────────

// Session holds the in-progress state for one chat conversation.
type Session struct {
	ID              string
	State           agent.CodeGenState
	Agent           *agent.CodeGenAgent
	EventCh         chan Event
	done            chan struct{}
	mu              sync.Mutex
	pendingApproval bool
}

// SessionManager keeps all active sessions in memory.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
	}
}

func (sm *SessionManager) Create(a *agent.CodeGenAgent) *Session {
	id := newID()
	s := &Session{
		ID:      id,
		Agent:   a,
		EventCh: make(chan Event, 32),
		done:    make(chan struct{}),
	}
	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()
	return s
}

func (sm *SessionManager) Get(id string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[id]
	return s, ok
}

func (sm *SessionManager) Remove(id string) {
	sm.mu.Lock()
	delete(sm.sessions, id)
	sm.mu.Unlock()
}

// ── Handlers ────────────────────────────────────────────────────────

type Handler struct {
	sm      *SessionManager
	agent   *agent.CodeGenAgent
	html    string // cached index.html
}

func New(sm *SessionManager, a *agent.CodeGenAgent, html string) *Handler {
	return &Handler{sm: sm, agent: a, html: html}
}

// HandleIndex serves the chat UI.
func (h *Handler) HandleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(h.html))
}

// HandleChat starts a code generation workflow.
// POST /api/chat  {"prompt": "..."}
func (h *Handler) HandleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if body.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}

	session := h.sm.Create(h.agent)

	// Run the workflow in a goroutine
	go session.runWorkflow(body.Prompt)

	writeJSON(w, http.StatusOK, map[string]string{"session_id": session.ID})
}

// HandleStream is the SSE endpoint.
// GET /api/stream?session=<id>
func (h *Handler) HandleStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}

	session, ok := h.sm.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-session.EventCh:
			if !ok {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
			flusher.Flush()
		}
	}
}

// HandleApprove approves the current code and runs the tester.
// POST /api/approve  {"session_id": "..."}
func (h *Handler) HandleApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	session, ok := h.sm.Get(body.SessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if !session.pendingApproval {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending approval"})
		return
	}

	session.pendingApproval = false

	// Approve in a goroutine so the SSE stream picks up the events
	go func() {
		result, err := session.Agent.Approve(context.Background(), session.State)
		if err != nil {
			session.EventCh <- Event{Type: "error", Error: err.Error()}
			close(session.EventCh)
			return
		}
		session.State = result

		session.EventCh <- Event{
			Type:    "test_result",
			Content: result.TestResult,
		}
		session.EventCh <- Event{
			Type:    "message",
			Content: "**Done!** Code generation and review complete.",
			Status:  "done",
		}
		close(session.EventCh)
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// HandleReject rejects the code with feedback and re-runs coder + reviewer.
// POST /api/reject  {"session_id": "...", "feedback": "..."}
func (h *Handler) HandleReject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Feedback  string `json:"feedback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	session, ok := h.sm.Get(body.SessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if !session.pendingApproval {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending approval"})
		return
	}

	session.pendingApproval = false
	session.State.HumanFeedback = body.Feedback

	go func() {
		result, err := session.Agent.Reject(context.Background(), session.State)
		if err != nil {
			session.EventCh <- Event{Type: "error", Error: err.Error()}
			close(session.EventCh)
			return
		}
		session.State = result
		session.pendingApproval = true

		session.EventCh <- Event{
			Type:    "message",
			Content: "✏️ Code revised based on your feedback. Review the new version below.",
		}
		session.EventCh <- Event{
			Type:    "code",
			Code:    result.Code,
			Content: "**Updated code:**",
		}
		session.EventCh <- Event{
			Type:    "message",
			Content: "**Review:**\n\n" + result.Review,
		}
		session.EventCh <- Event{
			Type:    "approval_needed",
			Content: "What do you think of the revised code?",
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// ── session internals ───────────────────────────────────────────────

func (s *Session) runWorkflow(prompt string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("session %s panic: %v", s.ID, r)
			s.EventCh <- Event{Type: "error", Error: fmt.Sprintf("internal error: %v", r)}
			close(s.EventCh)
		}
	}()

	state := agent.CodeGenState{
		Request:   prompt,
		Language:  "python",
		Status:    "started",
		Iteration: 1,
	}

	// Step 1: Generate (plan → code → review)
	result, err := s.Agent.Generate(context.Background(), state)
	if err != nil {
		s.EventCh <- Event{Type: "error", Error: err.Error()}
		close(s.EventCh)
		return
	}
	s.State = result

	// Step 2: Send progress events
	s.EventCh <- Event{Type: "step", Content: "Planning completed", Status: "planned"}
	s.EventCh <- Event{Type: "message", Content: "**Plan:**\n\n" + result.Plan}
	s.EventCh <- Event{Type: "step", Content: "Code written", Status: "coded"}
	s.EventCh <- Event{Type: "code", Code: result.Code, Content: "**Generated code:**"}
	s.EventCh <- Event{Type: "step", Content: "Review completed", Status: "reviewed"}
	s.EventCh <- Event{Type: "message", Content: "**Review:**\n\n" + result.Review}

	// Step 3: Request human approval
	s.mu.Lock()
	s.pendingApproval = true
	s.mu.Unlock()

	s.EventCh <- Event{
		Type:    "approval_needed",
		Content: "Review the code above. Approve to run tests, or reject with feedback for revision.",
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
