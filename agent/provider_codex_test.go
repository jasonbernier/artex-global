package agent

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/codexcli"
)

func TestCodexCLIConfig(t *testing.T) {
	t.Setenv("ARTEX_LLM_PROVIDER", "codex-cli")
	t.Setenv("ARTEX_LLM_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "unused-api-key")
	t.Setenv("ANTHROPIC_API_KEY", "unused-api-key")
	c, ok := FromEnv()
	if !ok || c.Format != codexcli.Format || !c.Configured() || c.APIKey != "" || c.Model != "" {
		t.Fatalf("env config=%+v, ok=%v", c, ok)
	}
	c = ConfigFrom("codex-cli", "test-model", "https://unused", "unused-key", "unused-proxy")
	if c.Provider() != "codex-cli" || c.Model != "test-model" || c.APIKey != "" || c.BaseURL != "" || c.Proxy != "" || !c.Configured() {
		t.Fatalf("profile config=%+v", c)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CODEX_BIN", bin)
	t.Setenv("ARTEX_CODEX_TIMEOUT", "1m")
	if _, err := c.NewProvider(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CODEX_TIMEOUT", "0s")
	if _, err := c.NewProvider(); err == nil {
		t.Fatal("accepted zero timeout")
	}
	t.Setenv("ARTEX_CODEX_TIMEOUT", "bad")
	if _, err := c.NewProvider(); err == nil {
		t.Fatal("accepted bad timeout")
	}
	if (Config{}).Configured() {
		t.Fatal("empty API provider configured")
	}
}
