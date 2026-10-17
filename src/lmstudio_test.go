// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// One request in LM Studio 0.4's server log, status lines as LM Studio writes them (runtime log verbosity high
// enough for llama.cpp's slot lines); the request body is made up. "SECRET" must never reach the screen.
var lmRequest = []string{
	`[2026-10-17 12:00:00][INFO][Client=lms-cli][Endpoint=listLoaded] Listing loaded models`,
	`[2026-10-17 12:00:01][DEBUG] Received request: POST to /v1/chat/completions with body  {`,
	`  "model": "google/gemma-4-e4b",`,
	`  "messages": [ { "role": "user", "content": "SECRET Prompt processing progress: 55.0% Finished streaming response" } ]`,
	`}`,
	`[2026-10-17 12:00:01][INFO][google/gemma-4-e4b] Running chat completion on conversation with 12 messages.`,
	`[2026-10-17 12:00:01][INFO][google/gemma-4-e4b] Streaming response...`,
	`[2026-10-17 12:00:01][DEBUG] 20.48.012.167 I slot launch_slot_: id  2 | task 5944 | processing task, is_child = 0`,
	`[2026-10-17 12:00:01][INFO][google/gemma-4-e4b] Prompt processing progress: 0.0%`,
	`[2026-10-17 12:00:02][INFO][google/gemma-4-e4b] Prompt processing progress: 36.8%`, // 9: reading
	`[2026-10-17 12:00:03][INFO][google/gemma-4-e4b] Prompt processing progress: 100.0%`,
	`[2026-10-17 12:00:06][DEBUG] 20.53.075.194 I slot print_timing: id  2 | task 5944 | n_gen =    245, tg =  81.10 t/s, tg_3s =  81.43 t/s`, // 11: writing
	`[2026-10-17 12:00:09][DEBUG] 21.03.760.462 I slot print_timing: id  2 | task 5944 | prompt eval time =    2054.35 ms /   817 tokens (    2.51 ms per token,   397.69 tokens per second)`,
	`21.03.760.467 I slot print_timing: id  2 | task 5944 |        eval time =   13693.85 ms /  1103 tokens (   12.43 ms per token,    80.47 tokens per second)`,
	`21.03.760.468 I slot print_timing: id  2 | task 5944 |       total time =   15748.20 ms /  1920 tokens`,
	`[2026-10-17 12:00:09][DEBUG] 21.03.760.711 I slot      release: id  2 | task 5944 | stop processing: n_tokens = 4874, truncated = 0`,
	`[2026-10-17 12:00:09][INFO][google/gemma-4-e4b] Finished streaming response`, // 16: idle
}

func TestLMLogStates(t *testing.T) {
	var s lmState
	now := time.Now()
	for i, l := range lmRequest {
		s.line(l+"\n", now)
		switch i {
		case 4:
			if s.Phase != "" {
				t.Fatalf("request body changed the state: %+v", s)
			}
		case 9:
			if s.Phase != "reading" || s.Pct != 36.8 || s.Model != "google/gemma-4-e4b" {
				t.Errorf("reading: %+v", s)
			}
		case 11:
			if s.Phase != "writing" || s.Decoded != 245 || s.GenRate != 81.43 || !s.Debug {
				t.Errorf("writing: %+v", s)
			}
		case 16:
			if s.Phase != "" || s.Prompt != 817 || s.ReadRate != 397.69 || s.Decoded != 1103 || s.Ctx != 4874 || s.Reqs != 1 {
				t.Errorf("done: %+v", s)
			}
		}
	}
	// INFO only (default verbosity): progress, no token counts
	var q lmState
	for _, l := range lmRequest {
		if strings.Contains(l, "[INFO]") {
			q.line(l, now)
		}
		if strings.Contains(l, "100.0%") && q.Phase != "writing" {
			t.Errorf("100%% should switch to writing: %+v", q)
		}
	}
	if q.Debug || q.Phase != "" || q.Reqs != 1 {
		t.Errorf("info only: %+v", q)
	}
}

func TestLMLogTail(t *testing.T) {
	d := t.TempDir()
	day1 := filepath.Join(d, "2026-10", "2026-10-16.1.log")
	write(t, day1, strings.Repeat("[2026-10-16 09:00:00][INFO] filler line that is long enough to matter\n", 5000)+
		strings.Join(lmRequest[:10], "\n")+"\n")
	l := &lmLog{dir: d}
	now := time.Now()
	st := l.tail(now)
	if !st.Found || st.Phase != "reading" || st.Pct != 36.8 {
		t.Fatalf("backfill from the end of a big log: %+v", st)
	}
	if l.off != int64(len(mustRead(t, day1))) {
		t.Error("offset not at the end")
	}
	// half a line, then the rest
	f, _ := os.OpenFile(day1, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(lmRequest[11][:40])
	f.Close()
	if st = l.tail(now); st.Phase != "reading" {
		t.Errorf("partial line applied: %+v", st)
	}
	f, _ = os.OpenFile(day1, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(lmRequest[11][40:] + "\n")
	f.Close()
	if st = l.tail(now); st.Phase != "writing" || st.Decoded != 245 {
		t.Errorf("joined line: %+v", st)
	}
	// next day: a new file, read from its start
	day2 := filepath.Join(d, "2026-10", "2026-10-17.1.log")
	write(t, day2, strings.Join(lmRequest[12:], "\n")+"\n")
	later := time.Now().Add(time.Second)
	os.Chtimes(day2, later, later)
	if st = l.tail(now); st.Phase != "" || st.Ctx != 4874 {
		t.Errorf("rotation: %+v", st)
	}
	// a request whose end was never logged times out
	l.st.Phase, l.st.since = "writing", now.Add(-20*time.Minute)
	if st = l.tail(now); st.Phase != "" {
		t.Error("stale request kept busy")
	}
	if st := (&lmLog{dir: filepath.Join(d, "none")}).tail(now); st.Found {
		t.Error("found a log in an empty dir")
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const lmModelsJSON = `{"models":[
 {"type":"llm","publisher":"google","key":"google/gemma-4-e4b","display_name":"Gemma 4 E4B","quantization":{"name":"Q4_K_M","bits_per_weight":4},
  "size_bytes":5000000000,"params_string":"4B","loaded_instances":[{"id":"google/gemma-4-e4b","config":{"context_length":32768,"parallel":4}}],"max_context_length":131072,"format":"gguf"},
 {"type":"llm","publisher":"qwen","key":"qwen/qwen3-8b","display_name":"Qwen3 8B","quantization":null,"size_bytes":1,"params_string":"8B",
  "loaded_instances":[{"id":"qwen/qwen3-8b:2","config":{"context_length":8192}}],"max_context_length":40960,"format":"gguf"},
 {"type":"embedding","publisher":"nomic","key":"text-embedding-nomic-embed-text-v1.5","display_name":"Nomic","quantization":null,"size_bytes":84000000,"params_string":null,
  "loaded_instances":[{"id":"text-embedding-nomic-embed-text-v1.5","config":{"context_length":2048}}],"max_context_length":2048,"format":"gguf"}]}`

func TestPickLM(t *testing.T) {
	if _, ok := pickLM([]byte(`{"data":[]}`), "", ""); ok {
		t.Error("llama-server style list taken for LM Studio")
	}
	cases := []struct{ want, logKey, key string }{
		{"", "", "google/gemma-4-e4b"},
		{"", "qwen/qwen3-8b", "qwen/qwen3-8b:2"},
		{"qwen/qwen3-8b:2", "google/gemma-4-e4b", "qwen/qwen3-8b:2"},
	}
	for _, c := range cases {
		p, ok := pickLM([]byte(lmModelsJSON), c.want, c.logKey)
		if !ok || p.Key != c.key || p.Loaded != 2 {
			t.Errorf("want=%q log=%q: %+v", c.want, c.logKey, p)
		}
	}
	if p, ok := pickLM([]byte(`{"models":[]}`), "", ""); !ok || p.Loaded != 0 {
		t.Errorf("empty: %+v", p)
	}
}

// mockLMS answers like LM Studio 0.4's server.
type mockLMS struct {
	mu     sync.Mutex
	token  string
	models string
	hits   map[string]int
}

func (m *mockLMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hits == nil {
		m.hits = map[string]int{}
	}
	m.hits[r.Method+" "+r.URL.Path]++
	if m.token != "" && r.Header.Get("Authorization") != "Bearer "+m.token {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"Unauthorized"}`)
		return
	}
	switch r.URL.Path {
	case "/api/v1/models":
		fmt.Fprint(w, m.models)
	case "/v1/models":
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	default:
		w.WriteHeader(200) // LM Studio answers unknown paths with an error JSON, never with llama.cpp's /health
		fmt.Fprint(w, `{"error":"Unexpected endpoint or method."}`)
	}
}

func TestLMStudioServer(t *testing.T) {
	m := &mockLMS{models: lmModelsJSON}
	srv := httptest.NewServer(m)
	defer srv.Close()
	logs := t.TempDir()
	logf := filepath.Join(logs, "2026-10", "2026-10-17.1.log")
	write(t, logf, lmRequest[0]+"\n")

	cfg := defaultConfig
	cfg.path = "test.json"
	cfg.LMLogs = logs
	st := &state{cfg: cfg, bases: []string{srv.URL}, known: map[string]*session{}}
	c := newClient([]string{srv.URL}, map[string]string{}, "", "auto", logs)
	now := time.Now()
	step := func(lines ...string) string {
		if len(lines) > 0 {
			f, _ := os.OpenFile(logf, os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString(strings.Join(lines, "\n") + "\n")
			f.Close()
		}
		now = now.Add(500 * time.Millisecond)
		st.srv.apply(c.round(now))
		out := frame(t, st, 120, 40)
		frame(t, st, 90, 40)
		if strings.Contains(out, "SECRET") {
			t.Fatal("chat text on screen")
		}
		return out
	}
	out := step()
	if st.srv.Kind != "lmstudio" || !strings.Contains(out, "IDLE") || !strings.Contains(out, "LM Studio · google/gemma-4-e4b") {
		t.Errorf("idle:\n%s", out)
	}
	if st.srv.Props.Gen.NCtx != 32768 || !strings.Contains(out, "/ 33k") {
		t.Errorf("context length from loaded_instances: %d", st.srv.Props.Gen.NCtx)
	}
	if out = step(lmRequest[1:10]...); !strings.Contains(out, "READING") || !strings.Contains(out, "37%") {
		t.Errorf("reading:\n%s", out)
	}
	if out = step(lmRequest[10:12]...); !strings.Contains(out, "WRITING") || !strings.Contains(out, "81.4 tok/s") {
		t.Errorf("writing:\n%s", out)
	}
	if out = step(lmRequest[12:]...); !strings.Contains(out, "IDLE") || !strings.Contains(out, "last request: 817 prompt at 398 tok/s") ||
		!strings.Contains(out, "4.9k / 33k") {
		t.Errorf("done:\n%s", out)
	}
	for k := range m.hits {
		if !strings.HasPrefix(k, "GET ") {
			t.Errorf("sent %s", k)
		}
	}

	// nothing loaded
	m.mu.Lock()
	m.models = `{"models":[{"type":"llm","key":"a","loaded_instances":[]}]}`
	m.mu.Unlock()
	now = now.Add(3 * time.Second)
	if out = step(); !strings.Contains(out, "NO MODEL") {
		t.Errorf("no model:\n%s", out)
	}

	// token required
	m.mu.Lock()
	m.token = "lm-secret"
	m.mu.Unlock()
	c2 := newClient([]string{srv.URL}, map[string]string{}, "", "auto", logs)
	st.srv = serverState{}
	st.srv.apply(c2.round(now))
	if out := frame(t, st, 120, 40); !strings.Contains(out, "AUTH") || !strings.Contains(out, "$LM_API_TOKEN") {
		t.Errorf("auth:\n%s", out)
	}
	c3 := newClient([]string{srv.URL}, map[string]string{"lmstudio": "lm-secret"}, "", "auto", logs)
	st.srv = serverState{}
	st.srv.apply(c3.round(now))
	if st.srv.Kind != "lmstudio" || st.srv.Auth {
		t.Errorf("with token: %+v", st.srv.Kind)
	}
}

// Nothing configured: llm-top tries llama-server's port, then LM Studio's, and keeps the one that answers.
func TestCandidates(t *testing.T) {
	lms := httptest.NewServer(&mockLMS{models: lmModelsJSON})
	defer lms.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	bases := []string{dead.URL, lms.URL}
	c := newClient(bases, map[string]string{}, "", "auto", t.TempDir())
	var s serverState
	now := time.Now()
	for i := 0; i < 4; i++ {
		s.apply(c.round(now))
		now = now.Add(500 * time.Millisecond)
	}
	if s.Kind != "lmstudio" || s.Base != lms.URL || !s.Up {
		t.Errorf("kind %q base %q up %v", s.Kind, s.Base, s.Up)
	}
}

func TestLMCompacting(t *testing.T) {
	var s lmState
	now := time.Now()
	body := []string{
		`[2026-10-17 12:00:01][DEBUG] Received request: POST to /v1/chat/completions with body  {`,
		`  "model": "qwen/qwen3-32b",`,
		`  "messages": [ { "role": "user", "content": "  \"max_tokens\": 20000," } ],`,
		`  "max_tokens": 20000,`,
		`  "stream": true`,
		`}`,
		`[2026-10-17 12:00:01][INFO][qwen/qwen3-32b] Running chat completion on conversation with 80 messages.`,
	}
	for _, l := range body {
		s.line(l, now)
	}
	st := serverState{Kind: "lmstudio", Up: true, LM: s}
	st.Props.Gen.NCtx = 131072
	if s.MaxTokens != 20000 || !st.compacting() {
		t.Errorf("compaction not seen: %+v", s)
	}
	// the next, normal request resets it
	for _, l := range []string{
		`[2026-10-17 12:01:00][INFO][qwen/qwen3-32b] Finished streaming response`,
		`[2026-10-17 12:01:01][DEBUG] Received request: POST to /v1/chat/completions with body  {`,
		`  "max_tokens": 65536,`, `}`,
		`[2026-10-17 12:01:01][INFO][qwen/qwen3-32b] Running chat completion on conversation with 3 messages.`,
	} {
		s.line(l, now)
	}
	st.LM = s
	if s.MaxTokens != 65536 || st.compacting() {
		t.Errorf("normal request taken for a compaction: %+v", s)
	}
}

func TestLMLogEdgeCases(t *testing.T) {
	now := time.Now()
	var s lmState
	s.line(`[2026-10-17 12:00:00][INFO][qwen/qwen3-32b] Running chat completion on conversation with 3 messages.`, now)
	// a client tag is not a model
	s.line(`[2026-10-17 12:00:00][INFO][Client=lms-cli][Endpoint=listLoaded] Listing loaded models`, now)
	if s.Model != "qwen/qwen3-32b" {
		t.Errorf("model taken from a client tag: %q", s.Model)
	}
	// text on a header line must not end the request or set numbers
	s.line(`[2026-10-17 12:00:01][INFO][qwen/qwen3-32b] Generated prediction: slot release: x | stop processing: n_tokens = 999`, now)
	s.line(`[2026-10-17 12:00:01][INFO] Received request: slot print_timing: id 0 | n_gen = 5, tg = 9.0 t/s`, now)
	if s.Phase != "reading" || s.Ctx != 0 || s.Decoded != 0 {
		t.Errorf("header text parsed as status: %+v", s)
	}
}

// A request with progress lines for 30 minutes stays busy; a new log file (LM Studio restarted) ends it.
func TestLMLogLongAndRestart(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "2026-10", "2026-10-17.1.log")
	write(t, f1, "")
	l := &lmLog{dir: dir}
	now := time.Now()
	l.tail(now)
	add := func(p, line string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		f.WriteString(line + "\n")
		f.Close()
	}
	add(f1, `[2026-10-17 12:00:00][INFO][m] Running chat completion on conversation with 3 messages.`)
	for i := 1; i <= 6; i++ {
		now = now.Add(5 * time.Minute)
		add(f1, fmt.Sprintf(`[2026-10-17 12:00:00][INFO][m] Prompt processing progress: %d.0%%`, i*10))
		if st := l.tail(now); st.Phase != "reading" {
			t.Fatalf("after %d min: phase %q", i*5, st.Phase)
		}
	}
	f2 := filepath.Join(dir, "2026-10", "2026-10-17.2.log")
	add(f2, `[2026-10-17 12:31:00][INFO] Server started.`)
	future := now.Add(time.Second)
	os.Chtimes(f2, future, future)
	if st := l.tail(now); st.Phase != "" {
		t.Errorf("phase %q carried into a new log file", st.Phase)
	}
}

// A chat in LM Studio's own window logs no API lines, only llama.cpp's slot lines, and early in a session their
// timestamps have one-digit hours (3.24.929.926).
var lmAppChat = []string{
	`[2026-10-17 13:04:34][DEBUG] 3.24.929.590 I slot get_availabl: id  0 | task -1 | selected slot by LCP similarity, f_sim_best = 0.995 (> 0.100 thold), f_keep = 1.000`,
	`[2026-10-17 13:04:34][DEBUG] 3.24.929.926 I slot launch_slot_: id  0 | task 2297 | processing task, is_child = 0`,                       // 1: reading
	`[2026-10-17 13:04:37][DEBUG] 3.28.112.331 I slot print_timing: id  0 | task 2297 | n_gen =    123, tg =  40.57 t/s, tg_3s =  40.90 t/s`, // 2: writing
	`[2026-10-17 13:04:47][DEBUG] 3.38.416.472 I slot print_timing: id  0 | task 2297 | prompt eval time =     174.89 ms /    11 tokens (   15.90 ms per token,    62.90 tokens per second)`,
	`3.38.416.477 I slot print_timing: id  0 | task 2297 |        eval time =   13311.50 ms /   544 tokens (   24.51 ms per token,    40.79 tokens per second)`,
	`3.38.416.478 I slot print_timing: id  0 | task 2297 |       total time =   13486.39 ms /   555 tokens`,
	`[2026-10-17 13:04:47][DEBUG] 3.38.416.573 I slot      release: id  0 | task 2297 | stop processing: n_tokens = 2861, truncated = 0`, // 6: idle
}

func TestLMAppChat(t *testing.T) {
	var s lmState
	now := time.Now()
	want := map[int]string{1: "reading", 2: "writing", 6: ""}
	for i, l := range lmAppChat {
		s.line(l, now)
		if ph, ok := want[i]; ok && s.Phase != ph {
			t.Errorf("after line %d: phase %q, want %q", i, s.Phase, ph)
		}
	}
	if !s.Debug || s.Decoded != 544 || s.Ctx != 2861 || s.Prompt != 11 || s.Reqs != 1 || s.GenRate < 40 {
		t.Errorf("%+v", s)
	}
}

// LM Studio logs every API request, so llm-top asks /api/v1/models only every lmModelsEvery and checks liveness
// with a bare TCP connect in between; a stopped LM Studio is still DOWN within a few rounds.
func TestLMQuietPolling(t *testing.T) {
	m := &mockLMS{models: lmModelsJSON}
	srv := httptest.NewServer(m)
	logs := t.TempDir()
	write(t, filepath.Join(logs, "2026-10", "2026-10-17.1.log"), "")
	cfg := defaultConfig
	cfg.path = "test.json"
	st := &state{cfg: cfg, bases: []string{srv.URL}, known: map[string]*session{}}
	c := newClient([]string{srv.URL}, map[string]string{}, "", "lmstudio", logs)
	now := time.Now()
	for i := 0; i < 40; i++ { // 20 s
		st.srv.apply(c.round(now))
		now = now.Add(500 * time.Millisecond)
	}
	m.mu.Lock()
	n := m.hits["GET /api/v1/models"]
	m.mu.Unlock()
	if n > 3 || !st.srv.Up || st.srv.Model == "" {
		t.Errorf("%d model list requests in 20 s, up=%v model=%q", n, st.srv.Up, st.srv.Model)
	}
	srv.Close()
	for i := 0; i < rotateAfter+1; i++ {
		st.srv.apply(c.round(now))
		now = now.Add(500 * time.Millisecond)
	}
	if st.srv.Up {
		t.Error("stopped LM Studio still up")
	}
}

// On startup the log's past requests are not shown as if they were current; a request that is still running is.
func TestLMBackfillCleanSlate(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "2026-10", "2026-10-17.1.log")
	write(t, f, strings.Join(lmAppChat, "\n")+"\n")
	l := &lmLog{dir: dir}
	st := l.tail(time.Now())
	if st.Phase != "" || st.Ctx != 0 || st.Prompt != 0 || st.Reqs != 0 || st.Decoded != 0 || !st.Debug {
		t.Errorf("finished request carried over: %+v", st)
	}
	write(t, f, strings.Join(lmAppChat[:3], "\n")+"\n") // ends mid-request
	l = &lmLog{dir: dir}
	if st := l.tail(time.Now()); st.Phase != "writing" || st.Decoded != 123 {
		t.Errorf("running request lost: %+v", st)
	}
}

// Another model loaded in LM Studio: the previous model's context and last request are dropped.
func TestLMModelSwitchClears(t *testing.T) {
	m := &mockLMS{models: lmModelsJSON}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := newClient([]string{srv.URL}, map[string]string{}, "", "lmstudio", t.TempDir())
	c.lmModel = "some/older-model"
	c.lm.st = lmState{Ctx: 5000, Prompt: 3000, ReadRate: 900, Reqs: 4, Debug: true}
	p := c.round(time.Now())
	if p.LMPick.Key == "" || p.LM.Ctx != 0 || p.LM.Prompt != 0 || p.LM.Reqs != 0 || !p.LM.Debug {
		t.Errorf("model %q: stats kept %+v", p.LMPick.Key, p.LM)
	}
}

// LM Studio idle with a model loaded: CONTEXT describes the model, ACTIVITY lists the server's requests; a chat in
// LM Studio's window adds one, and an old agent session does not come back while it runs.
func TestLMIdleAndRequestFeed(t *testing.T) {
	m := &mockLMS{models: lmModelsJSON}
	srv := httptest.NewServer(m)
	defer srv.Close()
	logs := t.TempDir()
	logf := filepath.Join(logs, "2026-10", "2026-10-17.1.log")
	write(t, logf, "")
	st := demoState(t)
	for _, s := range st.sessions {
		s.Mod, s.lastT = time.Now().Add(-48*time.Minute), time.Now().Add(-48*time.Minute)
	}
	st.bases, st.srv = []string{srv.URL}, serverState{}
	c := newClient([]string{srv.URL}, map[string]string{}, "", "lmstudio", logs)
	now := time.Now()
	step := func(lines ...string) string {
		if len(lines) > 0 {
			f, _ := os.OpenFile(logf, os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString(strings.Join(lines, "\n") + "\n")
			f.Close()
		}
		st.srv.apply(c.round(now))
		now = now.Add(500 * time.Millisecond)
		return frame(t, st, 120, 50)
	}
	out := step()
	for _, want := range []string{"model", "context", "no requests yet", "server requests since llm-top started"} {
		if !strings.Contains(out, want) {
			t.Errorf("idle: missing %q\n%s", want, out)
		}
	}
	out = step(lmAppChat[:3]...) // writing
	if !strings.Contains(out, "WRITING") || strings.Contains(out, "rocket-api ·") || strings.Contains(out, "read_file") {
		t.Errorf("busy: old session came back or not writing:\n%s", out)
	}
	out = step(lmAppChat[3:]...)
	if !strings.Contains(out, "→ 544 written") || !strings.Contains(out, "ctx 2.9k") || len(st.srv.Done) != 1 {
		t.Errorf("finished request not in ACTIVITY (%d done):\n%s", len(st.srv.Done), out)
	}
}

// A made-up LM Studio conversation file, in the shape LM Studio 0.4 saves (~/.lmstudio/conversations).
const lmConvJSON = `{"name":"Trip planning","tokenCount":1200,"lastUsedModel":{"identifier":"google/gemma-4"},"systemPrompt":"",
"messages":[
 {"currentlySelected":0,"versions":[{"type":"singleStep","role":"user","content":[{"type":"text","text":"SECRET plan a three day trip to Lisbon please"}]}]},
 {"currentlySelected":0,"versions":[{"type":"multiStep","role":"assistant","steps":[
   {"type":"contentBlock","shouldIncludeInContext":true,"style":{"type":"thinking"},"content":[{"type":"text","text":"SECRET thinking","tokensCount":120}]},
   {"type":"contentBlock","shouldIncludeInContext":true,"content":[{"type":"text","text":"SECRET day one","tokensCount":400}]},
   {"type":"debugInfoBlock"}]}]},
 {"currentlySelected":0,"versions":[{"type":"singleStep","role":"user","content":[{"type":"text","text":"SECRET shorter please"}]}]},
 {"currentlySelected":0,"versions":[{"type":"multiStep","role":"assistant","steps":[
   {"type":"contentBlock","shouldIncludeInContext":true,"content":[{"type":"text","text":"SECRET short","tokensCount":300}]}]}]}
]}`

func TestLMChatContext(t *testing.T) {
	mod := time.Now().Add(-time.Minute)
	s := parseConv([]byte(lmConvJSON), "c.json", mod)
	if s == nil || s.Project != "Trip planning" || s.turns != 2 || s.lastUsage.Prompt != 1200 || len(s.prompts) != 2 {
		t.Fatalf("%+v", s)
	}
	est := s.estimate(1200)
	if est[catThink] != 120 || est[catReply] != 700 || est[catUser] == 0 || est[catSys] <= 0 {
		t.Errorf("breakdown %v", est)
	}
	if s.prompts[0] >= s.prompts[1] || s.prompts[1] > 1200 {
		t.Errorf("prompt sizes %v", s.prompts)
	}
	// shown in CONTEXT when LM Studio is the backend and no agent is active; the text never reaches the screen
	st := demoState(t)
	for _, a := range st.sessions {
		a.Mod = time.Now().Add(-time.Hour)
	}
	st.lmChat = s
	st.srv = serverState{Polled: true, Kind: "lmstudio", Up: true, LMLoaded: 1, Model: "google/gemma-4", Info: modelInfo{Name: "Gemma 4"}}
	st.srv.Props.Gen.NCtx = 65536
	out := frame(t, st, 120, 50)
	for _, want := range []string{"what fills it · Trip planning", "1.2k / 66k", "Gemma 4", "LM Studio chat · 2 replies", "server requests"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET") {
		t.Error("chat text on screen")
	}
	if bad := parseConv([]byte(`{"messages":[]}`), "x", mod); bad != nil {
		t.Error("empty conversation parsed")
	}
}
