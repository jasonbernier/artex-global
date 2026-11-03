package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func testProvider(t *testing.T, mode string) *Provider {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Binary: bin, Timeout: 5 * time.Second, Model: "test-model", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	p.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Exercise real argv, stdin, file output, cancellation and event parsing
		// through a child test binary. No network or Codex account is required.
		return exec.CommandContext(ctx, name, append([]string{"-test.run=^TestCodexHelper$", "--", "__codex_helper__", mode}, args...)...)
	}
	return p
}

func TestCodexHelper(t *testing.T) {
	idx := -1
	for i, a := range os.Args {
		if a == "__codex_helper__" {
			idx = i
			break
		}
	}
	if idx == -1 {
		return
	}
	mode, args := os.Args[idx+1], os.Args[idx+2:]
	if mode == "hang" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	var output, dir, schema string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--output-last-message":
			output = args[i+1]
		case "--output-schema":
			schema = args[i+1]
		case "--cd":
			dir = args[i+1]
		}
	}
	if output == "" || schema == "" || dir == "" {
		os.Exit(3)
	}
	cwd, _ := os.Getwd()
	if !strings.EqualFold(cwd, dir) {
		os.Exit(4)
	}
	if _, err := os.ReadFile(schema); err != nil {
		os.Exit(5)
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		if os.Getenv(key) != "" {
			os.Exit(6)
		}
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if !strings.Contains(string(prompt), "PAYLOAD:") {
		os.Exit(7)
	}
	if mode == "fail" {
		io.WriteString(os.Stdout, "{\"type\":\"turn.failed\",\"error\":{\"message\":\"usage limit reached\"}}\n")
		os.Exit(1)
	}
	if mode == "partial" {
		os.Exit(0)
	}
	if mode == "missing" {
		io.WriteString(os.Stdout, "{\"type\":\"turn.completed\"}\n")
		os.Exit(0)
	}
	text := `{"text":"OK","tool_calls":[]}`
	if mode == "tool" {
		text = `{"text":"Checking","tool_calls":[{"name":"lookup","arguments":"{\"host\":\"lab.test\"}"}]}`
	}
	if mode == "invalid" {
		text = `{"text":"OK","tool_calls":[],"unexpected":true}`
	}
	if err := os.WriteFile(output, []byte(text), 0600); err != nil {
		os.Exit(8)
	}
	io.WriteString(os.Stdout, "{\"type\":\"thread.started\",\"thread_id\":\"isolated\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":100,\"cached_input_tokens\":20,\"output_tokens\":12}}\n")
	os.Exit(0)
}

func TestCompleteAndStream(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-reach-cli")
	t.Setenv("CODEX_API_KEY", "must-not-reach-cli")
	t.Setenv("OPENAI_BASE_URL", "https://must-not-reach-cli.invalid")
	for _, mode := range []string{"text", "tool"} {
		t.Run(mode, func(t *testing.T) {
			p := testProvider(t, mode)
			req := llm.CompletionRequest{System: []string{"system"}, Messages: []llm.Message{llm.UserText("hello")}, Tools: []llm.ToolSchema{{Name: "lookup"}}}
			msg, stop, usage, err := p.Complete(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if usage.InputTokens != 80 || usage.CacheReadTokens != 20 || usage.OutputTokens != 12 {
				t.Fatalf("usage=%+v", usage)
			}
			if mode == "tool" {
				if stop != "tool_use" || len(msg.ToolUses()) != 1 || string(msg.ToolUses()[0].Input) != `{"host":"lab.test"}` {
					t.Fatalf("bad tool turn: %+v, %s", msg, stop)
				}
			} else if stop != "end_turn" || msg.Text() != "OK" {
				t.Fatalf("bad text turn: %+v, %s", msg, stop)
			}
			acc := llm.NewAccumulator()
			for event, err := range p.Stream(context.Background(), req) {
				if err != nil {
					t.Fatal(err)
				}
				acc.Add(event)
			}
			if acc.Message().Text() != msg.Text() || acc.StopReason != stop || acc.Usage != usage || len(acc.Message().ToolUses()) != len(msg.ToolUses()) {
				t.Fatalf("stream differs: %+v", acc)
			}
		})
	}
}

func TestProcessFailures(t *testing.T) {
	for _, mode := range []string{"fail", "partial", "missing", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			p := testProvider(t, mode)
			_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
			if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "fail" && !strings.Contains(err.Error(), "usage limit") {
				t.Fatal(err)
			}
		})
	}
}

func TestCommandAndCleanup(t *testing.T) {
	p := testProvider(t, "text")
	command := p.command
	var argv []string
	var dir string
	p.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		argv = append([]string(nil), args...)
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--cd" {
				dir = args[i+1]
			}
		}
		return command(ctx, name, args...)
	}
	_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("private-prompt-sentinel")}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, arg := range []string{"--ignore-user-config", "--ephemeral", "read-only", `forced_login_method="chatgpt"`, "features.shell_tool=false"} {
		if !strings.Contains(joined, arg) {
			t.Fatalf("missing argument %s", arg)
		}
	}
	if strings.Contains(joined, "private-prompt-sentinel") || strings.Contains(joined, "dangerously") || argv[len(argv)-1] != "-" {
		t.Fatal("unexpected prompt or bypass in command arguments")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary request directory remains: %v", err)
	}
}

func TestCancellation(t *testing.T) {
	p := testProvider(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := p.Complete(ctx, llm.CompletionRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 4*time.Second {
		t.Fatalf("cancellation took %s: %v", time.Since(start), err)
	}
}

func TestDecodeRejectsMalformedTools(t *testing.T) {
	tools := []llm.ToolSchema{{Name: "allowed"}}
	for _, data := range []string{
		`{"text":"", "tool_calls":[{"name":"unknown","arguments":"{}"}]}`,
		`{"text":"", "tool_calls":[{"name":"allowed","arguments":"null"}]}`,
		`{"text":"", "tool_calls":[{"name":"allowed","arguments":"[]"}]}`,
		`{"text":"", "tool_calls":[{"name":"allowed","arguments":"bad-json"}]}`,
		`{"text":"", "tool_calls":[]}`, `{"text":"OK"}`, `{"text":"OK","tool_calls":null}`,
		`{"text":"OK","tool_calls":[]} {}`, `not json`,
	} {
		if _, _, err := decodeReply([]byte(data), tools); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestHistoryAndSchema(t *testing.T) {
	req := llm.CompletionRequest{
		System: []string{"host instructions"},
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Thinking: "secret-think-sentinel", Signature: "secret-signature-sentinel"},
				{Type: llm.BlockToolUse, ID: "call1", Name: "lookup", Input: json.RawMessage(`{"host":"lab.test"}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call1", "lookup failed", true)}},
		}, Tools: []llm.ToolSchema{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}},
	}
	prompt, schema, err := encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "secret-think-sentinel") || strings.Contains(prompt, "secret-signature-sentinel") {
		t.Fatal("thinking replayed")
	}
	for _, s := range []string{"call1", "lookup failed", `"is_error":true`, "host instructions", `"input_schema"`} {
		if !strings.Contains(prompt, s) {
			t.Fatalf("lost %s", s)
		}
	}
	if !json.Valid(schema) {
		t.Fatal("invalid schema")
	}
}

func TestSubscriptionEnvironment(t *testing.T) {
	env := []string{"Path=bin", "OPENAI_API_KEY=secret", "codex_api_key=secret", "OPENAI_BASE_URL=private", "CODEX_ACCESS_TOKEN=secret", "CODEX_HOME=/custom", "HOME=/user"}
	want := []string{"Path=bin", "CODEX_HOME=/custom", "HOME=/user"}
	if got := subscriptionEnv(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("env=%v", got)
	}
}

func TestUnexpectedCLIToolRejected(t *testing.T) {
	_, err := readEvents(strings.NewReader("{\"type\":\"item.started\",\"item\":{\"type\":\"command_execution\"}}\n"))
	if err == nil {
		t.Fatal("accepted CLI tool")
	}
}

func TestRateWaitCancellation(t *testing.T) {
	p := testProvider(t, "text")
	p.cfg.RatePerMinute = 1
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Set ARTEX_TEST_CODEX_LIVE=1 for an opt-in, harmless round trip using the
// existing subscription. Unit tests never consume subscription quota.
func TestLiveCLI(t *testing.T) {
	if os.Getenv("ARTEX_TEST_CODEX_LIVE") != "1" {
		t.Skip("set ARTEX_TEST_CODEX_LIVE=1 to use the signed-in CLI")
	}
	p, err := New(Config{Binary: os.Getenv("ARTEX_CODEX_BIN"), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	msg, stop, usage, err := p.Complete(context.Background(), llm.CompletionRequest{
		System: []string{"Reply with exactly OK. Do not call tools."}, Messages: []llm.Message{llm.UserText("ping")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(msg.Text()) != "OK" || stop != "end_turn" {
		t.Fatalf("unexpected reply: %+v, %s", msg, stop)
	}
	t.Logf("CLI signed-in round trip succeeded; usage=%+v", usage)
}

func TestLiveCLIToolRoundTrip(t *testing.T) {
	if os.Getenv("ARTEX_TEST_CODEX_LIVE") != "1" {
		t.Skip("set ARTEX_TEST_CODEX_LIVE=1 to use the signed-in CLI")
	}
	p, err := New(Config{Binary: os.Getenv("ARTEX_CODEX_BIN"), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.CompletionRequest{
		System:   []string{`Call lab_echo exactly once with text="demo". Once its result is in the conversation, reply exactly OK with no further calls.`},
		Messages: []llm.Message{llm.UserText("Test the provided echo tool.")},
		Tools: []llm.ToolSchema{{Name: "lab_echo", Description: "Returns the supplied text. This is a fake test tool with no side effects.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}, "additionalProperties": false,
		}}},
	}
	msg, stop, _, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if stop != "tool_use" || len(msg.ToolUses()) != 1 || msg.ToolUses()[0].Name != "lab_echo" {
		t.Fatalf("unexpected tool reply: %+v, %s", msg, stop)
	}
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.ToolUses()[0].Input, &args); err != nil || args.Text != "demo" {
		t.Fatalf("bad echo arguments: %s", msg.ToolUses()[0].Input)
	}
	req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(msg.ToolUses()[0].ID, args.Text, false)}})
	final, stop, _, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if stop != "end_turn" || strings.TrimSpace(final.Text()) != "OK" {
		t.Fatalf("tool result not handled: %+v, %s", final, stop)
	}
	t.Log("CLI tool request and host-supplied result round trip succeeded")
}
