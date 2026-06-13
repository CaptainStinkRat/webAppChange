package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Model (like Pydantic, but explicit struct + tags) ─────

type Task struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

// ── In-memory store (Python dict → Go map + mutex) ─────────
//
// Go doesn't have a GIL. If two requests hit the map at the same
// instant, we get a data race. The mutex serialises access — this
// is explicit in Go and invisible in Python.

var (
	mu     sync.RWMutex
	tasks  = map[int]Task{}
	nextID = 1
)

// ── App ────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/tasks", handleListTasks)
	mux.HandleFunc("POST /api/tasks", handleCreateTask)
	mux.HandleFunc("GET /api/tasks/{id}", handleGetTask)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      withLogging(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("Go API server starting on :8080")
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// ── Handlers ───────────────────────────────────────────────

// GET /api/tasks?completed=true
// Equivalent to FastAPI's Query(None) — manual extraction.
func handleListTasks(w http.ResponseWriter, r *http.Request) {
	completedFilter := strings.ToLower(r.URL.Query().Get("completed"))

	mu.RLock()
	defer mu.RUnlock()

	var result []Task
	for _, t := range tasks {
		switch completedFilter {
		case "true":
			if t.Completed {
				result = append(result, t)
			}
		case "false":
			if !t.Completed {
				result = append(result, t)
			}
		default:
			result = append(result, t)
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// POST /api/tasks
// Manual JSON body parsing — no Pydantic auto-validation.
func handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string `json:"title"`
		Completed bool   `json:"completed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON: " + err.Error(),
		})
		return
	}

	// In a real app you'd validate the title isn't empty here.
	// Go doesn't auto-validate — you handle it explicitly.

	mu.Lock()
	task := Task{
		ID:        nextID,
		Title:     body.Title,
		Completed: body.Completed,
	}
	tasks[nextID] = task
	nextID++
	mu.Unlock()

	writeJSON(w, http.StatusCreated, task)
}

// GET /api/tasks/{id}
// Path parameters are extracted from the request via .PathValue() (Go 1.22+).
func handleGetTask(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid task id",
		})
		return
	}

	mu.RLock()
	task, ok := tasks[id]
	mu.RUnlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Task not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// ── Helpers ────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ── Middleware (like FastAPI middleware, but a function wrapping a Handler) ─

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s — %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
	})
}
