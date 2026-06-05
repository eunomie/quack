// Package askmcp serves a single MCP tool, ask_user, over a localhost
// Streamable-HTTP endpoint. The agent calls it when it needs a decision only the
// session owner can make; the call blocks until the owner answers in Discord, so
// the agent waits for a real answer instead of self-resolving a headless
// AskUserQuestion. quack hosts one server for all sessions and tells which
// session is asking by a per-session token on the URL (?s=<token>).
package askmcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// Question is what the agent wants the owner to decide.
type Question struct {
	Header  string   // short label, e.g. "REPL display"
	Text    string   // the question itself
	Options []string // offered choices; the owner may also answer free-form
}

// Answer is the owner's reply, rendered back to the agent as the tool result.
type Answer struct {
	// Choice is the selected option text (or the owner's free-form reply). Empty
	// with Note set covers the timeout/unavailable fallback.
	Choice string
	// Note is extra context for the agent (e.g. "owner was unavailable; decide
	// yourself"). Optional.
	Note string
}

// AskFunc resolves a question for the session identified by token, blocking until
// the owner answers (or a timeout elapses). An error means the session couldn't
// be found or the wait was abandoned; the server reports it as a tool error.
type AskFunc func(ctx context.Context, token string, q Question) (Answer, error)

// Server is an http.Handler exposing the ask_user tool. Mount it on a localhost
// listener; ask is invoked for each tools/call.
type Server struct {
	ask AskFunc
}

func New(ask AskFunc) *Server { return &Server{ask: ask} }

// ServeHTTP implements the MCP Streamable-HTTP transport: JSON-RPC requests are
// POSTed and answered with a single application/json response (we never initiate
// server→client messages, so no SSE stream is needed). A GET opens the optional
// server→client stream, which we hold open but never write to.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Optional SSE stream; we have nothing to push, so just hold it open until
		// the client disconnects rather than 405 (some clients open it eagerly).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
		// handled below
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, errorResponse(nil, -32700, "parse error"))
		return
	}
	token := r.URL.Query().Get("s")

	// A notification (no id) gets no JSON-RPC response, just an accept.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.dispatch(r.Context(), token, req)
	writeJSON(w, resp)
}

func (s *Server) dispatch(ctx context.Context, token string, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return okResponse(req.ID, initializeResult(req.Params))
	case "ping":
		return okResponse(req.ID, map[string]any{})
	case "tools/list":
		return okResponse(req.ID, map[string]any{"tools": []any{askUserTool()}})
	case "tools/call":
		return s.callTool(ctx, token, req)
	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) callTool(ctx context.Context, token string, req rpcRequest) rpcResponse {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Header   string   `json:"header"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResponse(req.ID, -32602, "invalid params")
	}
	if p.Name != "ask_user" {
		return errorResponse(req.ID, -32602, "unknown tool: "+p.Name)
	}
	ans, err := s.ask(ctx, token, Question{
		Header:  p.Arguments.Header,
		Text:    p.Arguments.Question,
		Options: p.Arguments.Options,
	})
	if err != nil {
		// A tool error (isError) lets the agent recover rather than aborting.
		return okResponse(req.ID, toolText("could not reach the owner: "+err.Error(), true))
	}
	return okResponse(req.ID, toolText(renderAnswer(ans), false))
}

// renderAnswer turns the owner's reply into the text the agent reads back.
func renderAnswer(a Answer) string {
	switch {
	case a.Choice != "" && a.Note != "":
		return "The owner answered: " + a.Choice + "\n(" + a.Note + ")"
	case a.Choice != "":
		return "The owner answered: " + a.Choice
	case a.Note != "":
		return a.Note
	default:
		return "The owner did not answer."
	}
}

func askUserTool() map[string]any {
	return map[string]any{
		"name": "ask_user",
		"description": "Ask the session owner a question and wait for their answer. " +
			"Use this whenever you need a decision only the owner can make (a design " +
			"choice, a yes/no, picking between options) instead of guessing. The call " +
			"blocks until the owner replies in the Discord thread.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"header": map[string]any{
					"type":        "string",
					"description": "A short label for the question (a few words).",
				},
				"question": map[string]any{
					"type":        "string",
					"description": "The question to put to the owner.",
				},
				"options": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional choices to offer; the owner may also reply free-form.",
				},
			},
			"required": []string{"question"},
		},
	}
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

// initializeResult echoes the client's protocol version (falling back to a known
// one) and advertises the tools capability.
func initializeResult(params json.RawMessage) map[string]any {
	version := "2025-06-18"
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "quack-ask", "version": "0.1.0"},
	}
}

// JSON-RPC 2.0 plumbing.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func okResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeJSON(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
