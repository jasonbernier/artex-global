package db

import "testing"

// Checks the format migration, key clearing on a format switch and keyless pool
// eligibility together. Existing API profiles must continue to require keys.
func TestCodexProfileWithoutAPIKey(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()
	p := &LLMProfile{Name: "t-codex-keyless", Format: "openai", Model: "test-model", APIKey: "old-key", Streaming: true}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	p.ID, p.Format, p.APIKey = id, "codex-cli", ""
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	got, err := d.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "codex-cli" || got.APIKey != "" || got.APIKeyHint != "" {
		t.Fatalf("CLI format should not retain API credentials: format=%s, key=%t, hint=%q", got.Format, got.APIKey != "", got.APIKeyHint)
	}
	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range chain {
		if profile.ID == id {
			return
		}
	}
	t.Fatal("keyless Codex CLI profile absent from failover pool")
}
