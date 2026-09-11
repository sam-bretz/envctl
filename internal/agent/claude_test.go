package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestClaudeCredentialRejectsRefreshableLoginSessions(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if _, err := ClaudeCredential("", ""); err == nil || !strings.Contains(err.Error(), "setup-token") {
		t.Fatal("missing credential did not explain the supported sources", err)
	}
	if _, err := ClaudeCredential("", "keychain:claude-code"); err == nil {
		t.Fatal("interactive keychain session accepted")
	}
	dir := t.TempDir()
	session := filepath.Join(dir, "session.json")
	if err := os.WriteFile(session, []byte(`{"claudeAiOauth":{"accessToken":"private-access","refreshToken":"private-refresh"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ClaudeCredential(dir, "file:session.json")
	if err == nil || !strings.Contains(err.Error(), "refreshable") || strings.Contains(err.Error(), "private-") {
		t.Fatal("refreshable login session accepted or leaked", err)
	}
	token := filepath.Join(dir, "token.json")
	if err = os.WriteFile(token, []byte(`{"CLAUDE_CODE_OAUTH_TOKEN":"private-long-lived"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ClaudeCredential(dir, "file:token.json")
	if err != nil || c.OAuthToken != "private-long-lived" || len(c.AuthJSON) != 0 || !slices.Equal(c.Secrets, []string{"private-long-lived"}) {
		t.Fatal("long-lived token file was not resolved as a scoped secret", err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "private-env-token")
	if c, err = ClaudeCredential("", ""); err != nil || c.OAuthToken != "private-env-token" || c.APIKey != "" {
		t.Fatal("default did not select the OAuth token environment", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "private-api-key")
	if c, err = ClaudeCredential("", ""); err != nil || c.APIKey != "private-api-key" || c.OAuthToken != "" {
		t.Fatal("default did not prefer the API key environment", err)
	}
}

func TestClaudeRequestKeepsCredentialsInScopedEnvironment(t *testing.T) {
	i := Invocation{ID: "attempt_one", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "Implement the task", Schema: simpleSchema(), TimeoutSeconds: 300, Session: "019edc18-aaa1-755e-b930-980a2e70fede"}
	credential := Credential{OAuthToken: "private-token", Secrets: []string{"private-token"}}
	req, err := (Claude{}).Request(i, credential)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(req.Args, " "), credential.OAuthToken) || req.Env["CLAUDE_CODE_OAUTH_TOKEN"] != credential.OAuthToken || req.Env["ANTHROPIC_API_KEY"] != "" {
		t.Fatal("credential escaped the scoped environment")
	}
	if req.Env["CLAUDE_CONFIG_DIR"] != claudeHome("worker") || !slices.Equal(req.Secrets, credential.Secrets) || req.Input != i.Prompt {
		t.Fatal("role home, redaction or prompt input not scoped")
	}
	if at := slices.Index(req.Args, "--resume"); at < 0 || req.Args[at+1] != i.Session {
		t.Fatal("ambiguous session resume")
	}
}
