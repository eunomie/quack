package askmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpc posts a JSON-RPC request to the server and decodes the response.
func rpc(t *testing.T, h http.Handler, url, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); rec.Code == http.StatusOK && !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want json (code %d)", ct, rec.Code)
	}
	if rec.Code != http.StatusOK {
		return map[string]any{"_code": float64(rec.Code)}
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestInitializeAndToolsList(t *testing.T) {
	s := New(func(context.Context, string, Question) (Answer, error) { return Answer{}, nil })

	init := rpc(t, s, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	res, ok := init["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing: %v", init)
	}
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want echoed 2025-03-26", res["protocolVersion"])
	}

	list := rpc(t, s, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "ask_user" {
		t.Fatalf("tools/list = %v, want one ask_user tool", tools)
	}
}

func TestToolCallInvokesAskWithToken(t *testing.T) {
	var gotToken string
	var gotQ Question
	s := New(func(_ context.Context, token string, q Question) (Answer, error) {
		gotToken, gotQ = token, q
		return Answer{Choice: "option B"}, nil
	})

	resp := rpc(t, s, "/mcp?s=tok-123",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_user","arguments":{"question":"A or B?","options":["A","B"],"header":"pick"}}}`)

	if gotToken != "tok-123" {
		t.Errorf("token = %q, want tok-123", gotToken)
	}
	if gotQ.Text != "A or B?" || len(gotQ.Options) != 2 || gotQ.Header != "pick" {
		t.Errorf("question = %+v", gotQ)
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Errorf("isError = %v, want false", result["isError"])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "option B") {
		t.Errorf("tool result = %q, want it to carry the answer", text)
	}
}

func TestToolCallErrorIsToolError(t *testing.T) {
	s := New(func(context.Context, string, Question) (Answer, error) {
		return Answer{}, context.Canceled
	})
	resp := rpc(t, s, "/mcp",
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ask_user","arguments":{"question":"x?"}}}`)
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("a failed ask should be a tool error (isError=true), got %v", result)
	}
}

func TestNotificationGetsNoBody(t *testing.T) {
	s := New(func(context.Context, string, Question) (Answer, error) { return Answer{}, nil })
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("notification should have no body, got %q", rec.Body.String())
	}
}

func TestUnknownMethod(t *testing.T) {
	s := New(func(context.Context, string, Question) (Answer, error) { return Answer{}, nil })
	resp := rpc(t, s, "/mcp", `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	if resp["error"] == nil {
		t.Errorf("unknown method should return an error, got %v", resp)
	}
}
