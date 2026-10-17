// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCreatedWithDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "llm-top.json")
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.InPerM != defaultPrices.InPerM || c.Process != "llama-server" || c.path != p {
		t.Errorf("defaults: %+v", c)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal("not written:", err)
	}
	for _, k := range []string{`"server_url"`, `"api_key"`, `"input_per_m"`, `"sessions_dir"`, `"vram_gb"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("written file lacks %s", k)
		}
	}
	c2, err := loadConfig(p)
	if err != nil || c2.Help != c.Help || c2.priceCfg != c.priceCfg {
		t.Errorf("reload differs: %v", err)
	}
}

func TestConfigPartialWithBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "llm-top.json")
	// the 0.1.0-era file: prices only, written by PowerShell with a BOM
	write(t, p, "\xef\xbb\xbf{\"price_label\":\"Other\",\"input_per_m\":3,\"server_url\":\"10.0.0.5:8000\"}")
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Label != "Other" || c.InPerM != 3 || c.OutPerM != defaultPrices.OutPerM || c.HoldFile != "llm-watchdog.hold" {
		t.Errorf("merge: %+v", c)
	}
	if bases, _, _ := c.server(); len(bases) != 1 || bases[0] != "http://10.0.0.5:8000" {
		t.Errorf("bases %q", bases)
	}
}

func TestConfigBrokenIsKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "llm-top.json")
	write(t, p, `{"input_per_m": oops}`)
	c, err := loadConfig(p)
	if err == nil {
		t.Error("no error for broken JSON")
	}
	if c.InPerM != defaultPrices.InPerM {
		t.Error("broken file should fall back to defaults")
	}
	if b, _ := os.ReadFile(p); string(b) != `{"input_per_m": oops}` {
		t.Error("broken file was overwritten")
	}
}

func TestServerFromServerConfig(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "llm-top.json")
	write(t, p, `{}`)
	c, _ := loadConfig(p)
	if bases, alias, model := c.server(); strings.Join(bases, " ") != "http://127.0.0.1:8080 http://127.0.0.1:1234" || alias != "" || model != "" {
		t.Errorf("no server_config: %q %q %q", bases, alias, model)
	}
	c.Backend = "lmstudio"
	if bases, _, _ := c.server(); strings.Join(bases, " ") != "http://127.0.0.1:1234" {
		t.Errorf("lmstudio backend: %q", bases)
	}
	c.Backend = "auto"
	write(t, filepath.Join(d, "llm-server.json"), "\xef\xbb\xbf"+`{"port":8091,"alias":"agent","model":"D:\\m\\Qwen-27B.gguf"}`)
	if bases, alias, model := c.server(); strings.Join(bases, " ") != "http://127.0.0.1:8091" || alias != "agent" || model != "Qwen-27B" {
		t.Errorf("relative server_config: %q %q %q", bases, alias, model)
	}
	c.ServerURL = "https://gpu-box:9000/"
	if bases, alias, _ := c.server(); strings.Join(bases, " ") != "https://gpu-box:9000" || alias != "agent" {
		t.Errorf("server_url wins: %q %q", bases, alias)
	}
}

func TestConfigPathsAndKey(t *testing.T) {
	d := t.TempDir()
	c := defaultConfig
	c.path = filepath.Join(d, "llm-top.json")
	if got := c.rel("llm-watchdog.hold"); got != filepath.Join(d, "llm-watchdog.hold") {
		t.Errorf("rel: %q", got)
	}
	abs := filepath.Join(d, "x", "y.json")
	if got := c.rel(abs); got != abs {
		t.Errorf("abs: %q", got)
	}
	h, _ := os.UserHomeDir()
	if got := c.sessionsGlob(); got != filepath.Join(h, ".qwen", "projects", "*", "chats", "*.jsonl") {
		t.Errorf("default glob %q", got)
	}
	c.SessionsDir = "~/elsewhere"
	if got := c.sessionsGlob(); !strings.HasPrefix(got, filepath.Join(h, "elsewhere")) {
		t.Errorf("~ glob %q", got)
	}
	t.Setenv("LLAMA_API_KEY", "from-env")
	t.Setenv("LM_API_TOKEN", "lm-env")
	if k := c.apiKeys(); k["llama.cpp"] != "from-env" || k["lmstudio"] != "lm-env" {
		t.Errorf("env keys %v", k)
	}
	c.APIKey = "from-file"
	if k := c.apiKeys(); k["llama.cpp"] != "from-file" || k["lmstudio"] != "from-file" {
		t.Error("file key wins")
	}
	t.Setenv("LLMTOP_CONFIG", filepath.Join(d, "env.json"))
	if configPath("") != filepath.Join(d, "env.json") || configPath("x.json") != "x.json" {
		t.Error("configPath order")
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{`C:\Users\me\src\Proj X`: "Proj X", "/home/me/src/p/": "p", "plain": "plain", `D:\m\a.gguf`: "a.gguf"} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q", in, got)
		}
	}
}

func TestBackendAndRel(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "auto": "auto", "llama.cpp": "llama.cpp", "llamacpp": "llama.cpp",
		"llama-server": "llama.cpp", "LMStudio": "lmstudio", "lm studio": "lmstudio", "lms": "lmstudio", "ollama": "auto"} {
		if got := normBackend(in); got != want {
			t.Errorf("normBackend(%q) = %q, want %q", in, got, want)
		}
	}
	// backend lmstudio ignores the llama-server port in llm-server.json
	d := t.TempDir()
	write(t, filepath.Join(d, "llm-server.json"), `{"port": 8091}`)
	c := defaultConfig
	c.path, c.Backend = filepath.Join(d, "llm-top.json"), "LMStudio"
	if bases, _, _ := c.server(); len(bases) != 1 || !strings.HasSuffix(bases[0], ":1234") {
		t.Errorf("lmstudio bases %v", bases)
	}
	c.Backend = "auto"
	if bases, _, _ := c.server(); len(bases) != 1 || !strings.HasSuffix(bases[0], ":8091") {
		t.Errorf("auto bases %v", bases)
	}
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{"~": home, "~/x": filepath.Join(home, "x"), "/abs/p": "/abs/p", `\rooted`: `\rooted`,
		"rel": filepath.Join(d, "rel")} {
		if got := c.rel(in); got != want {
			t.Errorf("rel(%q) = %q, want %q", in, got, want)
		}
	}
}
