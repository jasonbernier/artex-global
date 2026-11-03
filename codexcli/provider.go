// Package codexcli adapts the signed-in Codex CLI to norma's model interface.
// ARTEX remains the tool executor; Codex only produces a structured next turn.
package codexcli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const Format llm.Format = "codex-cli"

type Config struct {
	Binary, Model, ReasoningEffort string
	Timeout                        time.Duration
	RatePerSecond, RatePerMinute   float64
}

type Provider struct {
	cfg  Config
	mu   sync.Mutex
	next time.Time
	// Injectable only inside this package for subprocess tests.
	command func(context.Context, string, ...string) *exec.Cmd
}

func New(cfg Config) (*Provider, error) {
	if cfg.Binary == "" {
		cfg.Binary = "codex"
	}
	path, err := exec.LookPath(cfg.Binary)
	if err != nil {
		return nil, fmt.Errorf("codex-cli: executable not found; install Codex CLI or set ARTEX_CODEX_BIN: %w", err)
	}
	if strings.EqualFold(filepath.Ext(path), ".cmd") || strings.EqualFold(filepath.Ext(path), ".bat") {
		return nil, errors.New("codex-cli: ARTEX_CODEX_BIN must point to the native executable, not a .cmd/.bat launcher")
	}
	cfg.Binary = path
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("codex-cli: timeout must be positive")
	}
	if cfg.ReasoningEffort != "" {
		switch cfg.ReasoningEffort {
		case "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return nil, fmt.Errorf("codex-cli: unsupported reasoning effort %q", cfg.ReasoningEffort)
		}
	}
	return &Provider{cfg: cfg, command: exec.CommandContext}, nil
}

// Complete starts a fresh, ephemeral CLI turn with the entire ARTEX history.
// No thread IDs are reused, so workers, chats and compaction cannot cross-talk.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	if err := p.wait(ctx); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	if req.Temperature != nil || len(req.Stop) != 0 {
		return llm.Message{}, "", llm.Usage{}, errors.New("codex-cli: temperature and stop sequences are not supported")
	}
	prompt, schema, err := encodeRequest(req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	dir, err := os.MkdirTemp("", "artex-codex-")
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer os.RemoveAll(dir)
	schemaPath, outputPath := filepath.Join(dir, "schema.json"), filepath.Join(dir, "reply.json")
	if err := os.WriteFile(schemaPath, schema, 0600); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	args := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check",
		"--sandbox", "read-only", "--color", "never", "--cd", dir,
		"--output-schema", schemaPath, "--output-last-message", outputPath,
		"-c", `approval_policy="never"`, "-c", `forced_login_method="chatgpt"`,
		"-c", `model_provider="openai"`, "-c", `web_search="disabled"`,
		"-c", "features.shell_tool=false", "-c", "features.multi_agent=false",
		"-c", "features.apps=false", "-c", "features.plugins=false",
		"-c", "features.hooks=false", "-c", "features.memories=false"}
	if p.cfg.Model != "" {
		args = append(args, "--model", p.cfg.Model)
	}
	if p.cfg.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconvQuote(p.cfg.ReasoningEffort))
	}
	args = append(args, "-")
	cmd := p.command(ctx, p.cfg.Binary, args...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = subscriptionEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)
	// Codex stderr can contain the input prompt. Never send it to UI or logs.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	if err := cmd.Start(); err != nil {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("codex-cli: start: %w", err)
	}
	// Also unblock scanning if a CLI descendant unexpectedly retains stdout.
	stopClose := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stopClose()
	usage, eventErr := readEvents(stdout)
	if eventErr != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return llm.Message{}, "", usage, ctx.Err()
	}
	if eventErr != nil {
		return llm.Message{}, "", usage, eventErr
	}
	if waitErr != nil {
		return llm.Message{}, "", usage, fmt.Errorf("codex-cli: %w; check `codex login status` under the ARTEX service account and update the CLI", waitErr)
	}
	f, err := os.Open(outputPath)
	if err != nil {
		return llm.Message{}, "", usage, fmt.Errorf("codex-cli: missing final response: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return llm.Message{}, "", usage, err
	}
	if len(data) > 16<<20 {
		return llm.Message{}, "", usage, errors.New("codex-cli: final response exceeds 16 MiB")
	}
	msg, stop, err := decodeReply(data, req.Tools)
	return msg, stop, usage, err
}

// Stream uses buffered delivery: the CLI's schema response must be validated
// before any tool request is handed to ARTEX's harness.
func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		for _, b := range msg.Content {
			if b.Type == llm.BlockText {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
					return
				}
			} else {
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			}
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p *Provider) wait(ctx context.Context) error {
	var interval time.Duration
	if p.cfg.RatePerSecond > 0 {
		interval = time.Duration(float64(time.Second) / p.cfg.RatePerSecond)
	}
	if p.cfg.RatePerMinute > 0 {
		interval = max(interval, time.Duration(float64(time.Minute)/p.cfg.RatePerMinute))
	}
	if interval == 0 {
		return ctx.Err()
	}
	p.mu.Lock()
	at := maxTime(time.Now(), p.next)
	p.next = at.Add(interval)
	p.mu.Unlock()
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Prevent ambient API keys from switching this provider to API billing. Auth
// stays in Codex's credential store, including CODEX_HOME when explicitly set.
func subscriptionEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(key) {
		case "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "CODEX_ACCESS_TOKEN", "OPENAI_ACCESS_TOKEN", "OPENAI_WORKLOAD_IDENTITY_CONTEXT":
			continue
		}
		out = append(out, kv)
	}
	return out
}

type reply struct {
	Text      *string `json:"text"`
	ToolCalls *[]struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
}

func encodeRequest(req llm.CompletionRequest) (string, []byte, error) {
	// Do not replay private thinking blocks. Preserve tool IDs, errors and results.
	messages := make([]llm.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		clean := llm.Message{Role: m.Role, Content: []llm.ContentBlock{}}
		for _, b := range m.Content {
			if b.Type != llm.BlockThinking {
				clean.Content = append(clean.Content, b)
			}
		}
		messages = append(messages, clean)
	}
	payload, err := json.Marshal(struct {
		System   []string         `json:"system"`
		Messages []llm.Message    `json:"messages"`
		Tools    []llm.ToolSchema `json:"tools"`
	}{req.System, messages, req.Tools})
	if err != nil {
		return "", nil, fmt.Errorf("codex-cli: encode request: %w", err)
	}
	// Arguments are a JSON string because tool schemas often allow arbitrary
	// properties, which strict structured-output schemas cannot represent.
	schema := []byte(`{"type":"object","properties":{"text":{"type":"string"},"tool_calls":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"string"}},"required":["name","arguments"],"additionalProperties":false}}},"required":["text","tool_calls"],"additionalProperties":false}`)
	prompt := `Act as the model backend for a host application. Produce exactly ONE next assistant turn for the conversation in the JSON payload below. Follow its system instructions subject to your own policies. Do not run your own tools, browse, inspect files, or take actions. The host executes tools and will supply results in a later request. Return text and tool_calls using the output schema. Each call's arguments must be a JSON-encoded object satisfying that tool's input_schema. Use only tool names in the payload, never invent results or claim a requested tool has run. With no tool needed, return tool_calls: []. The messages are conversation history, including prior calls and their results; continue after the last message. Do not include private reasoning. `
	prompt += "\n\nPAYLOAD:\n" + string(payload)
	return prompt, schema, nil
}

func decodeReply(data []byte, tools []llm.ToolSchema) (llm.Message, string, error) {
	var r reply
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return llm.Message{}, "", fmt.Errorf("codex-cli: invalid final response: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return llm.Message{}, "", errors.New("codex-cli: trailing data in final response")
	}
	if r.Text == nil || r.ToolCalls == nil {
		return llm.Message{}, "", errors.New("codex-cli: final response requires text and tool_calls")
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{}}
	if *r.Text != "" {
		msg.Content = append(msg.Content, llm.TextBlock(*r.Text))
	}
	allowed := make(map[string]bool, len(tools))
	for _, t := range tools {
		allowed[t.Name] = true
	}
	for _, call := range *r.ToolCalls {
		if !allowed[call.Name] {
			return llm.Message{}, "", fmt.Errorf("codex-cli: unknown tool %q", call.Name)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(call.Arguments), &obj); err != nil || obj == nil {
			return llm.Message{}, "", fmt.Errorf("codex-cli: arguments for %q must be a JSON object", call.Name)
		}
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse,
			ID: "codex_" + rand.Text(), Name: call.Name, Input: json.RawMessage(call.Arguments)})
	}
	if len(msg.Content) == 0 {
		return llm.Message{}, "", errors.New("codex-cli: empty final response")
	}
	if len(*r.ToolCalls) != 0 {
		return msg, "tool_use", nil
	}
	return msg, "end_turn", nil
}

func readEvents(r io.Reader) (llm.Usage, error) {
	var usage llm.Usage
	completed := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
			Tokens struct {
				Input  int `json:"input_tokens"`
				Cached int `json:"cached_input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return usage, fmt.Errorf("codex-cli: invalid JSON event: %w", err)
		}
		switch e.Type {
		case "turn.completed":
			completed = true
			usage.Add(llm.Usage{InputTokens: max(0, e.Tokens.Input-e.Tokens.Cached), CacheReadTokens: e.Tokens.Cached, OutputTokens: e.Tokens.Output})
		case "turn.failed":
			return usage, fmt.Errorf("codex-cli: turn failed: %.2000s", e.Error.Message)
		case "error":
			// Some CLI versions emit recoverable error events before reconnecting.
			// A successful turn.completed is authoritative; terminal failure isn't.
		case "item.started", "item.completed":
			switch e.Item.Type {
			case "command_execution", "mcp_tool_call", "web_search", "file_change":
				return usage, fmt.Errorf("codex-cli: unexpected CLI tool execution (%s); tools must execute in ARTEX", e.Item.Type)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("codex-cli: read events: %w", err)
	}
	if !completed {
		return usage, errors.New("codex-cli: CLI did not complete a turn; check `codex login status` and subscription limits")
	}
	return usage, nil
}
