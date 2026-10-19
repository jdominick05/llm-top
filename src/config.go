// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// Config: everything llm-top can't read from the machine or the server lives in llm-top.json.
//
// Lookup order: -config <path>, $LLMTOP_CONFIG, llm-top.json next to the executable (if it exists), then
// <user config dir>/llm-top/llm-top.json (%APPDATA% on Windows, ~/.config on Linux). The file is written with
// defaults only when it does not exist yet; missing fields keep their defaults. Relative paths inside it resolve
// against the file's own directory.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type config struct {
	ServerURL    string  `json:"server_url"`
	Backend      string  `json:"backend"`
	APIKey       string  `json:"api_key"`
	RouterModel  string  `json:"router_model"`
	ServerConfig string  `json:"server_config"`
	Process      string  `json:"server_process"`
	SessionsDir  string  `json:"sessions_dir"`
	HoldFile     string  `json:"hold_file"`
	LMLogs       string  `json:"lmstudio_logs"`
	VramGB       float64 `json:"vram_gb"`
	Context      int     `json:"context"`
	Help         string  `json:"_help"`
	OpenRGB      rgbCfg  `json:"openrgb"`
	priceCfg
	path string // the file this came from
}

var defaultConfig = config{
	Backend:      "auto",
	ServerConfig: "llm-server.json",
	Process:      "llama-server",
	HoldFile:     "llm-watchdog.hold",
	Help: "server_url: server base URL (empty = http://127.0.0.1:<port> with port from server_config, else :8080 " +
		"(llama-server) and :1234 (LM Studio) in turn). backend: auto, llama.cpp or lmstudio. api_key: sent as Bearer " +
		"token (empty = $LLAMA_API_KEY or $LM_API_TOKEN). router_model: model id to watch (llama-server router mode, " +
		"LM Studio with several loaded; empty = first loaded). server_config: optional JSON with port/alias/model. " +
		"sessions_dir: Qwen Code projects dir (empty = ~/.qwen/projects). hold_file: first line shown as HOLD while it " +
		"exists. lmstudio_logs: LM Studio server-logs dir (empty = ~/.lmstudio/server-logs). vram_gb / context: 0 = detect. " +
		"openrgb: light RGB devices by agent state through OpenRGB's SDK server (enabled, host, devices = name " +
		"substrings, empty = all in Direct mode; brightness 0-1; colors = phase -> #rrggbb).",
	OpenRGB:  rgbCfg{Host: "127.0.0.1:6742", Brightness: 1},
	priceCfg: defaultPrices,
}

// configPath picks the config file; explicit is "" when no -config was given.
func configPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("LLMTOP_CONFIG"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "llm-top.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "llm-top", "llm-top.json")
	}
	return "llm-top.json"
}

// loadConfig reads path, or writes the defaults there when it does not exist. A broken file is not overwritten.
func loadConfig(path string) (config, error) {
	c := defaultConfig
	c.path = path
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if b, err := json.MarshalIndent(defaultConfig, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &c); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) { // the decoder read every other field: only that one keeps its default
			return c, fmt.Errorf("%s: %s should be %s, not %s (using its default)", path, te.Field, jsonKind(te.Type), te.Value)
		}
		c = defaultConfig
		c.path = path
		return c, fmt.Errorf("%s: %v (using defaults)", path, err)
	}
	return c, nil
}

// jsonKind names what a config field takes, in JSON's words.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Struct, reflect.Map:
		return "an object"
	}
	return "a number"
}

// rel resolves a path from the config against the config file's directory; "~/" means the home dir.
func (c config) rel(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimLeft(p[1:], `/\`))
		}
	}
	// \dir on Windows is rooted on the current drive, not relative to the config
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/") {
		return p
	}
	return filepath.Join(filepath.Dir(c.path), p)
}

func (c config) sessionsGlob() string {
	d := c.rel(c.SessionsDir)
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".qwen", "projects")
	}
	return filepath.Join(d, "*", "chats", "*.jsonl")
}

// apiKeys gives the Bearer token per backend: api_key, else what each server reads itself.
func (c config) apiKeys() map[string]string {
	k := map[string]string{"llama.cpp": os.Getenv("LLAMA_API_KEY"), "lmstudio": os.Getenv("LM_API_TOKEN")}
	if c.APIKey != "" {
		k["llama.cpp"], k["lmstudio"] = c.APIKey, c.APIKey
	}
	return k
}

func (c config) lmLogs() string {
	if d := c.rel(c.LMLogs); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".lmstudio", "server-logs")
}

// server returns the URLs to try plus alias and model name from server_config, if any. Without server_url or a
// port in server_config, llama-server's and LM Studio's default ports are tried in turn.
// backend is the configured backend as "auto", "llama.cpp" or "lmstudio".
func (c config) backend() string { return normBackend(c.Backend) }

// normBackend accepts the usual spellings (llamacpp, llama-server, LMStudio, lms); anything else is "auto".
func normBackend(s string) string {
	switch strings.NewReplacer(" ", "", "-", "", "_", "", ".", "").Replace(strings.ToLower(s)) {
	case "llamacpp", "llama", "llamaserver":
		return "llama.cpp"
	case "lmstudio", "lms":
		return "lmstudio"
	}
	return "auto"
}

// lmConvs is LM Studio's conversations folder, next to its server-logs.
func (c config) lmConvs() string { return filepath.Join(filepath.Dir(c.lmLogs()), "conversations") }

func (c config) server() (bases []string, alias, model string) {
	port := 0
	if p := c.rel(c.ServerConfig); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var sc struct {
				Port  int    `json:"port"`
				Alias string `json:"alias"`
				Model string `json:"model"`
			}
			if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &sc) == nil {
				if sc.Port > 0 {
					port = sc.Port
				}
				alias = sc.Alias
				if sc.Model != "" {
					model = strings.TrimSuffix(baseName(sc.Model), ".gguf")
				}
			}
		}
	}
	base := strings.TrimRight(c.ServerURL, "/")
	switch {
	case base != "":
		if !strings.Contains(base, "://") {
			base = "http://" + base
		}
		bases = []string{base}
	case port > 0 && c.backend() != "lmstudio": // llm-server.json describes llama-server
		bases = []string{fmt.Sprintf("http://127.0.0.1:%d", port)}
	case c.backend() == "llama.cpp":
		bases = []string{"http://127.0.0.1:8080"}
	case c.backend() == "lmstudio":
		bases = []string{"http://127.0.0.1:1234"}
	default:
		bases = []string{"http://127.0.0.1:8080", "http://127.0.0.1:1234"}
	}
	return bases, alias, model
}

// baseName is filepath.Base for both separators: session logs and server configs may come from either OS.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}
