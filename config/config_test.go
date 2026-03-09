package config

import (
	"os"
	"testing"
)

func TestLoad_missingEnv(t *testing.T) {
	prev, had := os.LookupEnv("OPENTALON_MCP_SERVERS")
	if err := os.Unsetenv("OPENTALON_MCP_SERVERS"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	t.Cleanup(func() {
		if had {
			if err := os.Setenv("OPENTALON_MCP_SERVERS", prev); err != nil {
				t.Errorf("restore env: %v", err)
			}
		}
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when env not set")
	}
}

func TestLoad_emptyEnv(t *testing.T) {
	t.Setenv("OPENTALON_MCP_SERVERS", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestLoad_invalidJSON(t *testing.T) {
	t.Setenv("OPENTALON_MCP_SERVERS", "not-json")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_valid(t *testing.T) {
	t.Setenv("OPENTALON_MCP_SERVERS", `[{"server":"s1","url":"http://localhost/sse"}]`)
	cfgs, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("got %d configs, want 1", len(cfgs))
	}
	if cfgs[0].Server != "s1" || cfgs[0].URL != "http://localhost/sse" {
		t.Errorf("unexpected config: %+v", cfgs[0])
	}
}

func TestLoad_emptyArray(t *testing.T) {
	t.Setenv("OPENTALON_MCP_SERVERS", `[]`)
	cfgs, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfgs) != 0 {
		t.Errorf("got %d configs, want 0", len(cfgs))
	}
}

func TestLoad_headerEnvExpansion(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret123")
	t.Setenv("OPENTALON_MCP_SERVERS", `[{
		"server": "s",
		"url": "http://localhost/sse",
		"headers": {
			"Authorization": "Bearer {{env.MY_TOKEN}}",
			"X-Static": "fixed"
		}
	}]`)
	cfgs, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfgs[0].Headers["Authorization"], "Bearer secret123"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := cfgs[0].Headers["X-Static"], "fixed"; got != want {
		t.Errorf("X-Static = %q, want %q", got, want)
	}
}

func TestLoad_multipleServers(t *testing.T) {
	t.Setenv("OPENTALON_MCP_SERVERS", `[
		{"server":"a","url":"http://a/sse"},
		{"server":"b","url":"http://b/sse"}
	]`)
	cfgs, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configs, want 2", len(cfgs))
	}
	if cfgs[0].Server != "a" || cfgs[1].Server != "b" {
		t.Errorf("unexpected servers: %v, %v", cfgs[0].Server, cfgs[1].Server)
	}
}
