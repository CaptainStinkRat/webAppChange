// Package agent defines a LangGraph-based code generation workflow.
//
// The graph executes four nodes in sequence: planner → coder → reviewer → tester.
// Human-in-the-loop is handled using InvokeWithConfig with InterruptBefore,
// which pauses the graph before the tester node so the user can review
// generated code and either approve it (continue to tester) or reject it
// (loop back to coder with feedback).
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/smallnest/langgraphgo/graph"

	"webAppChange/app/internal/llm"
)

// ── State ────────────────────────────────────────────────────────────

// CodeGenState flows through every graph node. Fields accumulate as the
// workflow progresses — the state object is the single source of truth.
type CodeGenState struct {
	Request       string `json:"request"`
	Language      string `json:"language"`
	Plan          string `json:"plan"`
	Code          string `json:"code"`
	Review        string `json:"review"`
	TestResult    string `json:"test_result"`
	Status        string `json:"status"`
	Iteration     int    `json:"iteration"`
	Approved      bool   `json:"-"` // set by the human before resume
	HumanFeedback string `json:"-"` // set by the human on rejection
	StepOutput    string `json:"step_output,omitempty"`
}

// ── Graph builder ───────────────────────────────────────────────────

// CodeGenAgent wraps a compiled LangGraph runnable for the code-generation workflow.
type CodeGenAgent struct {
	runnable *graph.StateRunnable[CodeGenState]
	llm      *llm.Client
}

// New creates a CodeGenAgent, building and compiling the LangGraph.
func New(llmClient *llm.Client) (*CodeGenAgent, error) {
	b := graph.NewStateGraph[CodeGenState]()

	// ── Nodes ────────────────────────────────────────────────────────
	b.AddNode("planner", "planner", func(ctx context.Context, s CodeGenState) (CodeGenState, error) {
		if s.Plan != "" {
			return s, nil // already planned on a previous iteration
		}
		system := "You are a senior software engineer. Produce a concise, step-by-step plan for implementing the requested code. Return only the plan, no extra commentary."
		plan, err := llmClient.Chat(ctx, system, s.Request)
		if err != nil {
			return s, fmt.Errorf("planner: %w", err)
		}
		s.Plan = plan
		s.Status = "planned"
		s.StepOutput = "Plan created"
		return s, nil
	})

	b.AddNode("coder", "coder", func(ctx context.Context, s CodeGenState) (CodeGenState, error) {
		prompt := fmt.Sprintf("Request:\n%s\n\nPlan:\n%s", s.Request, s.Plan)
		if s.HumanFeedback != "" {
			prompt += fmt.Sprintf("\n\nPrevious iteration feedback (address these):\n%s", s.HumanFeedback)
		}
		system := "You are a senior software engineer. Write clean, well-documented code for the request. Follow the plan. Return only the code with brief inline comments, no extra explanation."
		code, err := llmClient.Chat(ctx, system, prompt)
		if err != nil {
			return s, fmt.Errorf("coder: %w", err)
		}
		s.Code = code
		s.Status = "coded"
		s.StepOutput = "Code written"
		return s, nil
	})

	b.AddNode("reviewer", "reviewer", func(ctx context.Context, s CodeGenState) (CodeGenState, error) {
		prompt := fmt.Sprintf("Review this code for correctness, edge cases, and style:\n\n%s\n\nRequest:\n%s", s.Code, s.Request)
		system := "You are a senior code reviewer. List specific issues, if any. If the code looks correct, say 'No issues found.' Be concise."
		review, err := llmClient.Chat(ctx, system, prompt)
		if err != nil {
			return s, fmt.Errorf("reviewer: %w", err)
		}
		s.Review = review
		s.Status = "reviewed"
		s.StepOutput = "Code reviewed"
		return s, nil
	})

	b.AddNode("tester", "tester", func(ctx context.Context, s CodeGenState) (CodeGenState, error) {
		prompt := fmt.Sprintf("Analyze this code and describe how you would test it. List 2-3 test cases or describe a test approach:\n\n%s", s.Code)
		system := "You are a QA engineer. Describe concrete test cases or a test plan for the code above. Be specific but keep it brief."
		testResult, err := llmClient.Chat(ctx, system, prompt)
		if err != nil {
			return s, fmt.Errorf("tester: %w", err)
		}
		s.TestResult = testResult
		s.Status = "tested"
		s.StepOutput = "Tests planned"
		return s, nil
	})

	// ── Edges ────────────────────────────────────────────────────────
	b.AddEdge("__start__", "planner")
	b.AddEdge("planner", "coder")
	b.AddEdge("coder", "reviewer")
	b.AddEdge("reviewer", "tester")
	b.AddEdge("tester", graph.END)
	b.SetEntryPoint("__start__")

	compiled, err := b.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile graph: %w", err)
	}

	return &CodeGenAgent{
		runnable: compiled,
		llm:      llmClient,
	}, nil
}

// extractInterruptState checks whether err is a GraphInterrupt and returns
// the state captured at the interruption point.  If the error is something
// else entirely, it returns the zero value and the original error.
func extractInterruptState(err error) (CodeGenState, bool) {
	var interrupt *graph.GraphInterrupt
	if errors.As(err, &interrupt) {
		if state, ok := interrupt.State.(CodeGenState); ok {
			return state, true
		}
	}
	return CodeGenState{}, false
}

// ── Execution helpers ───────────────────────────────────────────────

// Generate runs planner -> coder -> reviewer and stops before tester.
// On success the state has Plan, Code, and Review populated.
// The error returned is a *graph.GraphInterrupt (this is expected —
// it's the signal that human input is needed).
func (a *CodeGenAgent) Generate(ctx context.Context, state CodeGenState) (CodeGenState, error) {
	_, err := a.runnable.InvokeWithConfig(ctx, state, &graph.Config{
		InterruptBefore: []string{"tester"},
	})
	// The interrupt carries the accumulated state
	if result, ok := extractInterruptState(err); ok {
		return result, nil
	}
	return state, err // real error
}

// Approve runs the tester node and returns the final state.
func (a *CodeGenAgent) Approve(ctx context.Context, state CodeGenState) (CodeGenState, error) {
	state.Approved = true
	state.Status = "approved"
	state.StepOutput = "Human approved"

	result, err := a.runnable.InvokeWithConfig(ctx, state, &graph.Config{
		ResumeFrom: []string{"tester"},
	})
	if err != nil {
		return state, fmt.Errorf("approve: %w", err)
	}
	return result, nil
}

// Reject applies human feedback and runs coder -> reviewer again.
// Returns the state stopped before tester for another round of review.
func (a *CodeGenAgent) Reject(ctx context.Context, state CodeGenState) (CodeGenState, error) {
	state.Approved = false
	state.Iteration++
	state.Status = "rejected"
	state.StepOutput = "Revising with feedback"
	state.Review = "" // clear stale review

	_, err := a.runnable.InvokeWithConfig(ctx, state, &graph.Config{
		ResumeFrom:     []string{"coder"},
		InterruptBefore: []string{"tester"},
	})
	if result, ok := extractInterruptState(err); ok {
		return result, nil
	}
	return state, err
}
