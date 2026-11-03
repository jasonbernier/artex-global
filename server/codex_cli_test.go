package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexConnectionTestDoesNotRequireAPIKey(t *testing.T) {
	t.Setenv("ARTEX_CODEX_BIN", "nonexistent-artex-test-codex-executable")
	s := &Server{}
	r := httptest.NewRequest("POST", "/api/llm/test", strings.NewReader(`{"provider":"codex-cli"}`))
	w := httptest.NewRecorder()
	s.testLLM(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "executable not found") {
		t.Fatalf("should attempt CLI initialization without an API key, got %d: %s", w.Code, w.Body.String())
	}
}
