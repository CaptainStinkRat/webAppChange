// Command server is the entrypoint for the CodeGen web application.
//
// It wires together the LLM client, the LangGraph agent, HTTP handlers,
// and serves the chat interface on the configured port.
package main

import (
	"log"
	"net/http"
	"os"

	"webAppChange/app/internal/agent"
	"webAppChange/app/internal/handler"
	"webAppChange/app/internal/llm"
)

func main() {
	// ── Configuration from environment ───────────────────────────────
	ollamaURL := env("OLLAMA_URL", "http://ollama:11434")
	model := env("LLM_MODEL", "qwen3.5:9b")
	port := env("APP_PORT", "8080")

	// ── Dependencies ────────────────────────────────────────────────
	llmClient := llm.NewClient(ollamaURL, model)

	codeAgent, err := agent.New(llmClient)
	if err != nil {
		log.Fatalf("Failed to create code agent: %v", err)
	}

	sessionMgr := handler.NewSessionManager()

	// In production the HTML would be embedded; for learning purposes we
	// read the template file at startup.
	htmlBytes, err := os.ReadFile("web/templates/index.html")
	if err != nil {
		log.Fatalf("Failed to read index.html: %v", err)
	}

	h := handler.New(sessionMgr, codeAgent, string(htmlBytes))

	// ── Routes ───────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.HandleIndex)
	mux.HandleFunc("POST /api/chat", h.HandleChat)
	mux.HandleFunc("GET /api/stream", h.HandleStream)
	mux.HandleFunc("POST /api/approve", h.HandleApprove)
	mux.HandleFunc("POST /api/reject", h.HandleReject)

	// ── Start ────────────────────────────────────────────────────────
	addr := ":" + port
	log.Printf("CodeGen server starting on %s", addr)
	log.Printf("LLM: %s via %s", model, ollamaURL)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
