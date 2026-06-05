package askmcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAskUser_Integration drives the real claude CLI against the server: claude
// must call mcp__quack__ask_user, the server's AskFunc supplies an answer, and
// claude must report it back. This verifies the hand-rolled Streamable-HTTP
// transport actually interoperates with the MCP client.
func TestAskUser_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs an authenticated claude CLI)")
	}

	asked := make(chan Question, 1)
	srv := New(func(_ context.Context, token string, q Question) (Answer, error) {
		asked <- q
		return Answer{Choice: "BANANA"}, nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, srv)
	url := "http://" + ln.Addr().String() + "/mcp?s=tok-1"

	mcpCfg, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"quack": map[string]any{"type": "http", "url": url},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--output-format", "json",
		"--permission-mode", "acceptEdits",
		"--allowedTools", "mcp__quack__ask_user",
		"--mcp-config", string(mcpCfg),
		"--strict-mcp-config",
		"--model", "claude-haiku-4-5-20251001",
		"Call the mcp__quack__ask_user tool with question \"secret word?\" to ask the owner what to say, then reply with exactly the word they give you.",
	)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("claude failed: %v\n%s", err, out)
	}

	select {
	case q := <-asked:
		t.Logf("agent asked: %q (options=%v)", q.Text, q.Options)
	default:
		t.Fatalf("agent never called ask_user; output:\n%s", out)
	}

	var r struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(out, &r)
	if !strings.Contains(strings.ToUpper(r.Result), "BANANA") {
		t.Fatalf("agent did not report the owner's answer; result=%q\nfull=%s", r.Result, out)
	}
}
