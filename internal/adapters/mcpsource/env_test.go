package mcpsource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// envValue looks a variable up with the platform's case rules.
func envValue(values map[string]string, key string) (string, bool) {
	if value, ok := values[key]; ok {
		return value, true
	}
	if runtime.GOOS != "windows" {
		return "", false
	}
	for name, value := range values {
		if strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func envMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func TestMinimalChildEnvKeepsOnlyTheAllowlist(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/agent", "TMP=/tmp", "TEMP=/tmp",
		"SystemRoot=C:/Windows", "ComSpec=C:/Windows/system32/cmd.exe", "PATHEXT=.EXE;.CMD",
		"LANG=C.UTF-8", "LC_ALL=en_US.UTF-8",
		"BRAIN_TOKEN=brain-token", "BRAIN_DATABASE_URL=postgres://brain",
		"BRAIN_SECRET_SERVER_TOKEN=server-token", "DATABASE_URL=postgres://app",
		"GITHUB_TOKEN=ghp_example", "GITLAB_TOKEN=glpat-example",
		"AWS_ACCESS_KEY_ID=AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY=aws-secret",
		"OPENAI_API_KEY=sk-example", "ANTHROPIC_API_KEY=ant-example",
		"MY_SECRET=secret", "SECRET=secret", "APP_SECRET_KEY=secret",
		"TERM=xterm-256color", "NODE_OPTIONS=--max-old-space-size=4096",
	}
	StringEnvValue := func(name string) string {
		for _, entry := range base {
			key, value, _ := strings.Cut(entry, "=")
			if strings.EqualFold(key, name) {
				return value
			}
		}
		return ""
	}

	got, err := MinimalChildEnv(base, map[string]string{"PROVIDER_API_KEY": "explicit-value"})
	if err != nil {
		t.Fatalf("MinimalChildEnv: %v", err)
	}
	values := envMap(got)
	for _, key := range []string{"PATH", "HOME", "TMP", "TEMP", "SystemRoot", "ComSpec", "PATHEXT", "LANG", "LC_ALL"} {
		if value, ok := envValue(values, key); !ok || value != StringEnvValue(key) {
			t.Fatalf("%s = %q, %t; want the inherited value", key, value, ok)
		}
	}
	for _, key := range []string{
		"BRAIN_TOKEN", "BRAIN_DATABASE_URL", "BRAIN_SECRET_SERVER_TOKEN", "DATABASE_URL",
		"GITHUB_TOKEN", "GITLAB_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "MY_SECRET", "SECRET", "APP_SECRET_KEY",
		"TERM", "NODE_OPTIONS",
	} {
		if _, ok := envValue(values, key); ok {
			t.Fatalf("%s leaked into the child environment: %v", key, got)
		}
	}
	if value, ok := envValue(values, "PROVIDER_API_KEY"); !ok || value != "explicit-value" {
		t.Fatalf("explicit variable = %q, %t", value, ok)
	}
}

func TestMinimalChildEnvExplicitValueWins(t *testing.T) {
	got, err := MinimalChildEnv([]string{"PATH=/usr/bin", "HOME=/home/agent"}, map[string]string{"PATH": "/opt/tools", "API_TOKEN": "x"})
	if err != nil {
		t.Fatalf("MinimalChildEnv: %v", err)
	}
	values := envMap(got)
	if value, _ := envValue(values, "PATH"); value != "/opt/tools" {
		t.Fatalf("PATH = %q, want the explicit value", value)
	}
	if len(got) != 3 {
		t.Fatalf("environment = %v, want exactly PATH, HOME and API_TOKEN", got)
	}
}

func TestMinimalChildEnvRejectsInvalidNames(t *testing.T) {
	for _, key := range []string{"", "1BAD", "bad-name", "bad name", "bad=name", strings.Repeat("A", 200)} {
		if _, err := MinimalChildEnv(nil, map[string]string{key: "x"}); err == nil {
			t.Fatalf("MinimalChildEnv accepted the variable name %q", key)
		}
	}
}

func TestParseChildEnvBounds(t *testing.T) {
	valid := `{"API_KEY":"value","PRIVATE_TOKEN":"other"}`
	values, err := parseChildEnv(valid)
	if err != nil || len(values) != 2 {
		t.Fatalf("parseChildEnv = %v, %v", values, err)
	}
	if values, err := parseChildEnv(""); err != nil || values != nil {
		t.Fatalf("empty env = %v, %v", values, err)
	}
	tooMany := make(map[string]string, maximumChildEnvEntries+1)
	for index := 0; index <= maximumChildEnvEntries; index++ {
		tooMany["KEY_"+strings.Repeat("A", index)] = "x"
	}
	encoded, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"not json":    "API_KEY=value",
		"not objects": `["API_KEY"]`,
		"too many":    string(encoded),
		"newline":     `{"API_KEY":"line\nbreak"}`,
		"bad name":    `{"bad-name":"x"}`,
		"null value":  `{"API_KEY":null}`,
	} {
		if _, err := parseChildEnv(raw); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	oversized := `{"API_KEY":"` + strings.Repeat("x", maximumChildEnvBytes) + `"}`
	if _, err := parseChildEnv(oversized); err == nil {
		t.Fatal("an oversized env document was accepted")
	}
}

// TestStdioChildDoesNotInheritSecrets launches the test binary as a stdio MCP
// server and inspects the environment it actually received.
func TestStdioChildDoesNotInheritSecrets(t *testing.T) {
	secrets := map[string]string{
		"BRAIN_TOKEN":           "brain-token-value",
		"BRAIN_DATABASE_URL":    "postgres://brain-secret",
		"BRAIN_SECRET_API":      "server-secret-value",
		"AWS_SECRET_ACCESS_KEY": "aws-secret-value",
		"AWS_ACCESS_KEY_ID":     "AKIAEXAMPLE",
		"DATABASE_URL":          "postgres://app-secret",
		"GITHUB_TOKEN":          "ghp_example",
		"OPENAI_API_KEY":        "sk-example",
	}
	for key, value := range secrets {
		t.Setenv(key, value)
	}
	dump := filepath.Join(t.TempDir(), "child-env.json")
	explicit, err := json.Marshal(map[string]string{
		"LV24_MCP_ENV_HELPER": "1",
		"LV24_MCP_ENV_DUMP":   dump,
		"CHILD_API_KEY":       "explicit-child-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := stdioConnection(map[string]string{"tools": "stdio_search"})
	credentials := stdioCredentials{
		command: os.Args[0],
		args:    "-test.run=^TestMCPStdioEnvHelper$",
		env:     string(explicit),
	}
	source, err := NewSource(conn, credentials, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read via stdio: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items = %+v, want one", batch.Items)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the child did not dump its environment: %v", err)
	}
	var child map[string]string
	if err := json.Unmarshal(raw, &child); err != nil {
		t.Fatalf("child environment is not JSON: %v", err)
	}
	for key := range secrets {
		if _, ok := envValue(child, key); ok {
			t.Fatalf("%s leaked into the stdio child", key)
		}
	}
	if value, ok := envValue(child, "CHILD_API_KEY"); !ok || value != "explicit-child-value" {
		t.Fatalf("explicit variable = %q, %t", value, ok)
	}
	if value, _ := envValue(child, "PATH"); value != os.Getenv("PATH") {
		t.Fatalf("PATH = %q, want the parent value", value)
	}
}

// TestMCPStdioEnvHelper is the child of TestStdioChildDoesNotInheritSecrets. It
// writes its environment to LV24_MCP_ENV_DUMP and then serves one tool.
func TestMCPStdioEnvHelper(t *testing.T) {
	if os.Getenv("LV24_MCP_ENV_HELPER") != "1" {
		return
	}
	values := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("LV24_MCP_ENV_DUMP"), raw, 0o600); err != nil {
		os.Exit(2)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-env-test", Version: "1"}, nil)
	addServerTool(server, "stdio_search", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: []any{map[string]any{"id": "stdio-1", "title": "stdio item"}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
