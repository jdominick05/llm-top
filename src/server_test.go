// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ---------- mock llama-server ----------

type mockModel struct{ id, status string }

// mock answers like llama-server started with a given set of flags.
type mock struct {
	key       string      // --api-key
	health    int         // 0 = 200; 503 = loading
	noSlots   bool        // --no-slots
	noMetrics bool        // no --metrics
	sleeping  bool        // --sleep-idle-seconds, asleep
	router    []mockModel // router mode (--models-dir), nil = single model
	slots     string
	metrics   string
	props     string

	mu   sync.Mutex
	hits map[string]int // path -> GETs; non-GETs are counted under "!"; "path?model" for router questions
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.hits == nil {
		m.hits = map[string]int{}
	}
	if r.Method != http.MethodGet {
		m.hits["!"]++
	}
	m.hits[r.URL.Path]++
	if md := r.URL.Query().Get("model"); md != "" {
		m.hits[r.URL.Path+"?"+md]++
	}
	m.mu.Unlock()
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":{"code":%d,"message":%q}}`, code, msg)
	}
	switch r.URL.Path {
	case "/health":
		if m.health == 503 {
			fail(503, "Loading model")
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	case "/models", "/v1/models":
		if m.router == nil {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"single","object":"model"}]}`)
			return
		}
		var parts []string
		for _, md := range m.router {
			parts = append(parts, fmt.Sprintf(`{"id":%q,"object":"model","status":{"value":%q}}`, md.id, md.status))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(parts, ","))
		return
	}
	if m.key != "" && r.Header.Get("Authorization") != "Bearer "+m.key {
		fail(401, "Invalid API Key")
		return
	}
	if m.router != nil {
		want := r.URL.Query().Get("model")
		ok := false
		for _, md := range m.router {
			ok = ok || (md.id == want && (md.status == "loaded" || md.status == "sleeping"))
		}
		if !ok {
			fail(400, "model name is missing from the request")
			return
		}
	}
	switch r.URL.Path {
	case "/props":
		p := m.props
		if p == "" {
			p = `{"default_generation_settings":{"n_ctx":32768},"total_slots":1,"model_alias":"","model_path":"/m/tiny.gguf"}`
		}
		asleep := m.sleeping
		for _, md := range m.router {
			asleep = asleep || md.id == r.URL.Query().Get("model") && md.status == "sleeping"
		}
		if asleep {
			p = strings.Replace(p, `"total_slots"`, `"is_sleeping":true,"total_slots"`, 1)
		}
		fmt.Fprint(w, p)
	case "/metrics":
		if m.noMetrics {
			fail(501, "This server does not support metrics endpoint.")
			return
		}
		fmt.Fprint(w, m.metrics)
	case "/slots":
		if m.noSlots {
			fail(501, "This server does not support slots endpoint.")
			return
		}
		fmt.Fprint(w, m.slots)
	default:
		http.NotFound(w, r)
	}
}

func (m *mock) count(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

// slot builds one /slots entry; obj = old next_token object instead of the current array.
func slot(id int, busy bool, task, prompt, done, cache, decoded int, obj bool) string {
	nt := fmt.Sprintf(`{"has_next_token":true,"n_remain":-1,"n_decoded":%d}`, decoded)
	if !obj {
		nt = "[" + nt + "]"
	}
	return fmt.Sprintf(`{"id":%d,"n_ctx":32768,"is_processing":%t,"id_task":%d,"n_prompt_tokens":%d,"n_prompt_tokens_processed":%d,"n_prompt_tokens_cache":%d,"next_token":%s}`,
		id, busy, task, prompt, done, cache, nt)
}

func slotsJSON(s ...string) string { return "[" + strings.Join(s, ",") + "]" }

func metricsText(processing int) string {
	return fmt.Sprintf("# HELP x\nllamacpp:prompt_tokens_total 1000\nllamacpp:prompt_tokens_cached_total 9000\n"+
		"llamacpp:prompt_seconds_total 2\nllamacpp:tokens_predicted_total 500\nllamacpp:tokens_predicted_seconds_total 20\n"+
		"llamacpp:requests_processing %d\nllamacpp:requests_deferred 0\nllamacpp:n_tokens_max 4000\n"+
		"llamacpp:spec_decode_num_accepted_tokens_per_pos_total{position=\"0\"} 7\n", processing)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// run polls the server like pollServer does, with a fake clock, and returns the state.
func run(t *testing.T, url, key, want string, rounds int, step func(i int)) *state {
	t.Helper()
	cfg := defaultConfig
	cfg.path = "test.json"
	st := &state{cfg: cfg, bases: []string{url}, known: map[string]*session{}}
	c := newClient([]string{url}, map[string]string{"llama.cpp": key, "lmstudio": key}, want, "auto", t.TempDir())
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < rounds; i++ {
		if step != nil {
			step(i)
		}
		st.srv.apply(c.round(now))
		now = now.Add(500 * time.Millisecond)
	}
	return st
}

// frame renders at w x h and checks that no line is wider than the terminal.
func frame(t *testing.T, st *state, w, h int) string {
	t.Helper()
	out := model{st: st, w: w, h: h}.render()
	for i, l := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("%dx%d: line %d is %d wide: %q", w, h, i, lw, ansi.Strip(l))
		}
	}
	return ansi.Strip(out)
}

func header(t *testing.T, st *state) string {
	out := frame(t, st, 120, 40)
	frame(t, st, 90, 40)
	frame(t, st, 200, 60)
	lines := strings.SplitN(out, "\n", 3)
	return lines[0] + "\n" + lines[1]
}

// ---------- scenarios ----------

func TestLiveFixture(t *testing.T) {
	m := &mock{slots: readFixture(t, "slots.json"), metrics: readFixture(t, "metrics.txt"), props: readFixture(t, "props.json")}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 6, nil)
	h := header(t, st)
	if !strings.Contains(h, "READING") || !strings.Contains(h, "qwen3.8-agent") || !strings.Contains(h, "Qwen3.8-27B-R3-mix") {
		t.Errorf("header:\n%s", h)
	}
	if st.srv.Props.Gen.NCtx != 229376 || st.srv.Metrics["n_tokens_max"] != 186269 {
		t.Errorf("props/metrics not parsed: %+v %v", st.srv.Props, st.srv.Metrics["n_tokens_max"])
	}
	if m.count("!") != 0 {
		t.Error("sent a non-GET request")
	}
}

func TestIdleDoesNotPollSlots(t *testing.T) {
	m := &mock{slots: slotsJSON(slot(0, false, 5, 1200, 1200, 0, 40, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 20, nil)
	if h := header(t, st); !strings.Contains(h, "IDLE") {
		t.Errorf("header:\n%s", h)
	}
	// one look at startup; after that /metrics says idle, so the idle timer of --sleep-idle-seconds can run out
	if n := m.count("/slots"); n != 1 {
		t.Errorf("/slots polled %d times while idle, want 1", n)
	}
	if st.srv.Slot.NPrompt != 1200 {
		t.Errorf("startup slot picture missing: %+v", st.srv.Slot)
	}
}

func TestShortRequestBetweenScrapes(t *testing.T) {
	m := &mock{slots: slotsJSON(slot(0, false, 5, 1200, 1200, 0, 40, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 10, func(i int) {
		if i == 5 { // finished before any scrape saw requests_processing > 0
			m.mu.Lock()
			m.metrics = strings.Replace(metricsText(0), "tokens_predicted_total 500", "tokens_predicted_total 530", 1)
			m.slots = slotsJSON(slot(0, false, 6, 1500, 300, 1200, 30, false))
			m.mu.Unlock()
		}
	})
	if n := m.count("/slots"); n != 2 {
		t.Errorf("/slots asked %d times, want startup + one look after the counters moved", n)
	}
	if st.srv.Slot.Task != 6 || st.srv.Slot.NPrompt != 1500 {
		t.Errorf("slot picture not refreshed: %+v", st.srv.Slot)
	}
}

func TestBusyPollsSlotsAndMeasuresRates(t *testing.T) {
	m := &mock{metrics: metricsText(1)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 8, func(i int) {
		m.mu.Lock()
		m.slots = slotsJSON(slot(0, true, 9, 20000, 1000*(i+1), 0, 0, false)) // 1000 tokens per 0.5 s
		m.mu.Unlock()
	})
	if h := header(t, st); !strings.Contains(h, "READING") {
		t.Errorf("header:\n%s", h)
	}
	if r := st.srv.curRead; r < 1900 || r > 2100 {
		t.Errorf("read rate %.0f, want ~2000 tok/s", r)
	}
	if n := m.count("/slots"); n < 7 {
		t.Errorf("/slots polled %d times while busy", n)
	}
}

func TestWritingOldNextTokenObject(t *testing.T) {
	m := &mock{metrics: metricsText(1)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 6, func(i int) {
		m.mu.Lock()
		m.slots = slotsJSON(slot(0, true, 3, 5000, 5000, 4000, 10*(i+1), true))
		m.mu.Unlock()
	})
	if h := header(t, st); !strings.Contains(h, "WRITING") {
		t.Errorf("header:\n%s", h)
	}
	if g := st.srv.curGen; g < 19 || g > 21 {
		t.Errorf("write rate %.1f, want ~20 tok/s", g)
	}
}

func TestNoMetrics(t *testing.T) {
	m := &mock{noMetrics: true, slots: slotsJSON(slot(0, false, 1, 900, 900, 0, 12, false))}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 10, nil)
	out := frame(t, st, 120, 40)
	if !strings.Contains(out, "IDLE") || !strings.Contains(out, "no /metrics") {
		t.Errorf("frame:\n%s", out)
	}
	if n := m.count("/slots"); n != 10 {
		t.Errorf("without /metrics /slots is the only signal: polled %d of 10 rounds", n)
	}
	if n := m.count("/metrics"); n > 2 {
		t.Errorf("/metrics retried %d times in 10 rounds (5 s)", n)
	}
	if st.srv.Metrics != nil {
		t.Error("metrics without --metrics")
	}
}

func TestNoSlotsEndpoint(t *testing.T) {
	m := &mock{noSlots: true, metrics: metricsText(1)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 6, nil)
	if h := header(t, st); !strings.Contains(h, "BUSY") {
		t.Errorf("header:\n%s", h)
	}
	m.metrics = metricsText(0)
	st2 := run(t, srv.URL, "", "", 6, nil)
	if h := header(t, st2); !strings.Contains(h, "IDLE") || !strings.Contains(h, "--no-slots") {
		t.Errorf("header:\n%s", h)
	}
	if st2.srv.Props.Gen.NCtx != 32768 {
		t.Errorf("n_ctx from /props: %d", st2.srv.Props.Gen.NCtx)
	}
}

func TestAPIKey(t *testing.T) {
	m := &mock{key: "s3cret", slots: slotsJSON(slot(0, false, 1, 10, 10, 0, 1, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	if h := header(t, run(t, srv.URL, "", "", 4, nil)); !strings.Contains(h, "AUTH") {
		t.Errorf("no key:\n%s", h)
	}
	if h := header(t, run(t, srv.URL, "wrong", "", 4, nil)); !strings.Contains(h, "AUTH") {
		t.Errorf("wrong key:\n%s", h)
	}
	if h := header(t, run(t, srv.URL, "s3cret", "", 4, nil)); !strings.Contains(h, "IDLE") {
		t.Errorf("right key:\n%s", h)
	}
}

func TestLoadingAndDown(t *testing.T) {
	m := &mock{health: 503}
	srv := httptest.NewServer(m)
	st := run(t, srv.URL, "", "", 3, nil)
	if h := header(t, st); !strings.Contains(h, "LOADING") {
		t.Errorf("header:\n%s", h)
	}
	if m.count("/slots")+m.count("/props") != 0 {
		t.Error("asked more than /health while loading")
	}
	srv.Close()
	st = run(t, srv.URL, "", "", 3, nil)
	if h := header(t, st); !strings.Contains(h, "DOWN") {
		t.Errorf("header:\n%s", h)
	}
}

func TestMultiSlot(t *testing.T) {
	m := &mock{metrics: metricsText(2)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 6, func(i int) {
		m.mu.Lock()
		m.slots = slotsJSON(
			slot(0, false, 1, 100, 100, 0, 5, false),
			slot(1, false, 2, 100, 100, 0, 5, false),
			slot(2, true, 7, 8000, 8000, 7000, 10*(i+1), false),
			slot(3, true, 8, 9000, 500*(i+1), 0, 0, false),
		)
		m.mu.Unlock()
	})
	h := header(t, st)
	if !strings.Contains(h, "WRITING") || !strings.Contains(h, "slot id 2 · 4 slots, 2 busy") {
		t.Errorf("header:\n%s", h)
	}
	// both busy slots count towards throughput: slot 3 reads 1000/s, slot 2 writes 20/s
	if st.srv.curRead < 900 || st.srv.curGen < 19 {
		t.Errorf("rates read %.0f write %.1f", st.srv.curRead, st.srv.curGen)
	}
}

func TestRouter(t *testing.T) {
	m := &mock{router: []mockModel{{"big-model", "unloaded"}, {"small-model", "loaded"}},
		slots: slotsJSON(slot(0, false, 1, 50, 50, 0, 3, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 4, nil)
	if h := header(t, st); !strings.Contains(h, "small-model") || !strings.Contains(h, "IDLE") {
		t.Errorf("auto pick:\n%s", h)
	}
	before := m.count("/slots") + m.count("/props") + m.count("/metrics")
	st = run(t, srv.URL, "", "big-model", 4, nil)
	if h := header(t, st); !strings.Contains(h, "ROUTER") || !strings.Contains(h, "big-model is unloaded") {
		t.Errorf("unloaded router_model:\n%s", h)
	}
	if after := m.count("/slots") + m.count("/props") + m.count("/metrics"); after != before {
		t.Error("asked an unloaded model (that could load it)")
	}
	m.router = []mockModel{{"big-model", "unloaded"}}
	if h := header(t, run(t, srv.URL, "", "", 3, nil)); !strings.Contains(h, "no model loaded") {
		t.Errorf("nothing loaded:\n%s", h)
	}
}

// An evicted model is never asked about again: /models is read every round in router mode, so not one ?model=
// question follows the round that saw it go (with autoload, one could load it back and evict the new one).
func TestRouterEviction(t *testing.T) {
	m := &mock{router: []mockModel{{"a", "loaded"}, {"b", "unloaded"}},
		slots: slotsJSON(slot(0, false, 1, 50, 50, 0, 3, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	asked := func(md string) int { return m.count("/props?"+md) + m.count("/metrics?"+md) + m.count("/slots?"+md) }
	var atFlip int
	st := run(t, srv.URL, "", "", 20, func(i int) {
		if i == 7 {
			atFlip = asked("a")
			m.router = []mockModel{{"a", "unloaded"}, {"b", "loaded"}}
		}
	})
	if atFlip == 0 {
		t.Fatal("a was never watched")
	}
	if n := asked("a"); n != atFlip {
		t.Errorf("%d questions about a after it was unloaded", n-atFlip)
	}
	if h := header(t, st); !strings.Contains(h, "b") || asked("b") == 0 {
		t.Errorf("did not move to b:\n%s", h)
	}
}

// Moving to a model that sleeps never asks its /slots (that would wake it), and the old model's token counters
// don't make the new one look busy.
func TestRouterSwitchToSleeping(t *testing.T) {
	m := &mock{router: []mockModel{{"a", "loaded"}, {"b", "sleeping"}},
		slots: slotsJSON(slot(0, false, 1, 50, 50, 0, 3, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	run(t, srv.URL, "", "", 20, func(i int) {
		if i == 7 {
			m.router = []mockModel{{"a", "unloaded"}, {"b", "sleeping"}}
			m.metrics = strings.Replace(metricsText(0), "tokens_predicted_total 500", "tokens_predicted_total 9500", 1) // b's counters differ from a's
		}
	})
	if n := m.count("/slots?b"); n != 0 {
		t.Errorf("%d /slots questions to the sleeping model", n)
	}
	if m.count("/metrics?b") == 0 {
		t.Error("b was never watched")
	}
}

func TestSleeping(t *testing.T) {
	m := &mock{sleeping: true, slots: slotsJSON(slot(0, false, 1, 50, 50, 0, 3, false)), metrics: metricsText(0)}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 10, nil)
	if h := header(t, st); !strings.Contains(h, "SLEEP") {
		t.Errorf("header:\n%s", h)
	}
	if n := m.count("/slots"); n != 0 {
		t.Errorf("/slots (which wakes the server) asked %d times while asleep", n)
	}
}

func TestUnknownContextSize(t *testing.T) {
	m := &mock{noSlots: true, noMetrics: true, props: `{"total_slots":1}`}
	srv := httptest.NewServer(m)
	defer srv.Close()
	out := frame(t, run(t, srv.URL, "", "", 3, nil), 120, 40)
	if !strings.Contains(out, "131k?") {
		t.Errorf("unknown n_ctx should be marked:\n%s", out)
	}
}

func TestRouterTarget(t *testing.T) {
	cases := []struct {
		body, want, target, note string
		router, asleep           bool
	}{
		{`{"data":[{"id":"a","object":"model"}]}`, "", "", "", false, false},
		{`{"data":[]}`, "", "", "", false, false},
		{`not json`, "", "", "", false, false},
		{`{"data":[{"id":"a","status":{"value":"loading"}},{"id":"b","status":{"value":"sleeping"}}]}`, "", "b", "", true, true},
		{`{"data":[{"id":"a","status":{"value":"loaded"}},{"id":"b","status":{"value":"sleeping"}}]}`, "b", "b", "", true, true},
		{`{"data":[{"id":"a","status":{"value":"loaded"}},{"id":"b","status":{"value":"sleeping"}}]}`, "", "a", "", true, false},
		{`{"data":[{"id":"a","status":{"value":"loaded"}}]}`, "x", "", "x not found", true, false},
		{`{"data":[{"id":"a","status":{"value":"downloading"}}]}`, "", "", "no model loaded", true, false},
	}
	for _, c := range cases {
		r, tg, n, a := routerTarget([]byte(c.body), c.want)
		if r != c.router || tg != c.target || n != c.note || a != c.asleep {
			t.Errorf("%s want=%q: got %v %q %q asleep=%v", c.body, c.want, r, tg, n, a)
		}
	}
}

func TestParseMetrics(t *testing.T) {
	m := parseMetrics(strings.NewReader(readFixture(t, "metrics.txt")))
	if m["prompt_tokens_total"] != 1.40275e+06 || m["requests_processing"] != 1 {
		t.Errorf("%v", m)
	}
	for k := range m {
		if strings.Contains(k, "{") || strings.HasPrefix(k, "llamacpp:") {
			t.Errorf("bad key %q", k)
		}
	}
}

// slotP is slot() with the request's n_predict, which is how a Qwen Code compaction is recognised.
func slotP(nPredict int, s string) string {
	return strings.Replace(s, `"next_token"`, fmt.Sprintf(`"params":{"n_predict":%d},"next_token"`, nPredict), 1)
}

func TestCompacting(t *testing.T) {
	cases := []struct {
		name     string
		slot     string
		want     string
		agentNow bool
	}{
		{"normal turn", slotP(65536, slot(0, true, 4, 30000, 9000, 20000, 0, false)), "READING", false},
		{"compaction reading", slotP(20000, slot(0, true, 5, 30000, 9000, 0, 0, false)), "COMPACTING", true},
		{"compaction writing", slotP(20000, slot(0, true, 5, 30000, 30000, 0, 800, false)), "writing the summary", true},
		{"clamped near a full window", slotP(1500, slot(0, true, 6, 31000, 100, 0, 0, false)), "COMPACTING", true},
		{"small max_tokens, small prompt", slotP(1500, slot(0, true, 7, 2000, 100, 0, 0, false)), "READING", false},
	}
	for _, c := range cases {
		m := &mock{metrics: metricsText(1), slots: slotsJSON(c.slot)}
		srv := httptest.NewServer(m)
		st := run(t, srv.URL, "", "", 4, nil)
		st.sessions = []*session{newSession("x", "proj")}
		st.sessions[0].Mod = time.Now() // Qwen Code writes the session log as it sends the request
		h := header(t, st)
		if !strings.Contains(h, c.want) {
			t.Errorf("%s: want %q in\n%s", c.name, c.want, h)
		}
		if c.want == "READING" && strings.Contains(h, "COMPACTING") {
			t.Errorf("%s: shown as compaction", c.name)
		}
		if got := strings.Contains(frame(t, st, 120, 40), "compacting: the model summarizes"); got != c.agentNow {
			t.Errorf("%s: AGENTS now line compacting=%v", c.name, got)
		}
		srv.Close()
	}
}

// ---------- regressions ----------

// stepper drives one client against changing servers, like pollServer.
type stepper struct {
	t   *testing.T
	st  *state
	c   *srvClient
	now time.Time
}

func newStepper(t *testing.T, urls ...string) *stepper {
	cfg := defaultConfig
	cfg.path = "test.json"
	c := newClient(urls, map[string]string{}, "", "auto", t.TempDir())
	c.cl.Timeout = 150 * time.Millisecond
	return &stepper{t: t, st: &state{cfg: cfg, bases: urls, known: map[string]*session{}}, c: c,
		now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

func (s *stepper) step(n int) string {
	for i := 0; i < n; i++ {
		s.st.srv.apply(s.c.round(s.now))
		s.now = s.now.Add(500 * time.Millisecond)
	}
	return header(s.t, s.st)
}

// A server that dies mid-request: nothing may keep saying busy (AGENTS spinner, COMPACTING) while it is DOWN.
func TestDownClearsBusy(t *testing.T) {
	m := &mock{metrics: metricsText(1), slots: slotsJSON(slotP(20000, slot(0, true, 5, 30000, 9000, 0, 0, false)))}
	srv := httptest.NewServer(m)
	s := newStepper(t, srv.URL)
	s.st.sessions = []*session{newSession("x", "proj")}
	s.st.sessions[0].Mod = time.Now() // Qwen Code writes the session log as it sends the request
	if h := s.step(4); !strings.Contains(h, "COMPACTING") {
		t.Fatalf("not busy first:\n%s", h)
	}
	srv.Close()
	h := s.step(rotateAfter + 1)
	if !strings.Contains(h, "DOWN") || s.st.srv.busy() || s.st.srv.compacting() || s.st.srv.Metrics != nil {
		t.Errorf("down but busy=%v compacting=%v metrics=%v\n%s", s.st.srv.busy(), s.st.srv.compacting(), s.st.srv.Metrics != nil, h)
	}
	if f := frame(t, s.st, 120, 40); strings.Contains(f, "compacting") {
		t.Errorf("AGENTS still compacting while down")
	}
}

// One answer slower than the client timeout (a saturated server) must not flash DOWN, lose the model or restart
// the "for X" clock; a server that stays silent is DOWN after rotateAfter rounds.
func TestSlowAnswerNoFlash(t *testing.T) {
	var slow sync.Map
	m := &mock{metrics: metricsText(1), slots: slotsJSON(slot(0, true, 5, 30000, 9000, 0, 0, false))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := slow.Load("on"); ok && r.URL.Path == "/health" {
			time.Sleep(300 * time.Millisecond)
		}
		m.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s := newStepper(t, srv.URL, "http://127.0.0.1:1")
	s.step(4)
	since := s.st.srv.busySince
	slow.Store("on", true)
	if h := s.step(1); !strings.Contains(h, "READING") || s.st.srv.busySince != since || s.c.base != srv.URL {
		t.Errorf("one slow answer: base %s\n%s", s.c.base, h)
	}
	slow.Delete("on")
	if h := s.step(1); !strings.Contains(h, "READING") || s.st.srv.busySince != since {
		t.Errorf("after the slow answer:\n%s", h)
	}
	slow.Store("on", true)
	if h := s.step(rotateAfter); !strings.Contains(h, "DOWN") {
		t.Errorf("silent server not DOWN:\n%s", h)
	}
}

// Two busy slots; when the shown one finishes, the other is shown with its own elapsed time.
func TestBusySinceSlotSwitch(t *testing.T) {
	m := &mock{metrics: metricsText(2), slots: slotsJSON(slot(0, true, 1, 9000, 100, 0, 0, false), slot(1, true, 2, 9000, 100, 0, 0, false))}
	srv := httptest.NewServer(m)
	defer srv.Close()
	s := newStepper(t, srv.URL)
	s.step(1)
	t0 := s.st.srv.busySince
	s.step(20)
	m.mu.Lock()
	m.slots = slotsJSON(slot(0, false, 1, 9000, 9000, 0, 50, false), slot(1, true, 2, 9000, 4000, 0, 0, false))
	m.mu.Unlock()
	s.step(1)
	if s.st.srv.Slot.ID != 1 || !s.st.srv.busySince.Equal(t0) {
		t.Errorf("shown slot %d, busy since %v, want %v", s.st.srv.Slot.ID, s.st.srv.busySince, t0)
	}
}

// A server started with -n 20000 gives every request that budget: that is not a compaction.
func TestCompactingServerDefault(t *testing.T) {
	m := &mock{metrics: metricsText(1), slots: slotsJSON(slotP(20000, slot(0, true, 5, 30000, 9000, 0, 0, false))),
		props: `{"default_generation_settings":{"n_ctx":32768,"params":{"n_predict":20000}},"total_slots":1,"model_path":"/m/x.gguf"}`}
	srv := httptest.NewServer(m)
	defer srv.Close()
	st := run(t, srv.URL, "", "", 4, nil)
	if h := header(t, st); strings.Contains(h, "COMPACTING") || !strings.Contains(h, "READING") {
		t.Errorf("header:\n%s", h)
	}
}

// A finished llama-server request lands in the request feed with its prompt, cache and output.
func TestRequestFeedLlama(t *testing.T) {
	m := &mock{metrics: metricsText(1), slots: slotsJSON(slot(0, true, 7, 9000, 2000, 6000, 0, false))}
	srv := httptest.NewServer(m)
	defer srv.Close()
	s := newStepper(t, srv.URL)
	s.step(2)
	m.mu.Lock()
	m.slots = slotsJSON(slot(0, true, 7, 9000, 3000, 6000, 40, false))
	m.mu.Unlock()
	s.step(4)
	m.mu.Lock()
	m.slots, m.metrics = slotsJSON(slot(0, false, 7, 9000, 3000, 6000, 120, false)), metricsText(0)
	m.mu.Unlock()
	s.step(2)
	if len(s.st.srv.Done) != 1 {
		t.Fatalf("%d requests recorded", len(s.st.srv.Done))
	}
	e := s.st.srv.Done[0]
	if e.Prompt != 9000 || e.Cached != 6000 || e.Out != 120 || e.Dur <= 0 || e.GenRate <= 0 {
		t.Errorf("%+v", e)
	}
	if out := frame(t, s.st, 120, 40); !strings.Contains(out, "request 3.0k new + 6.0k cached") || !strings.Contains(out, "→ 120 written") {
		t.Errorf("feed:\n%s", out)
	}
}

// Nothing on the default candidates, but the server process listens elsewhere: the client tries that address.
func TestDiscoveredPort(t *testing.T) {
	m := &mock{metrics: metricsText(0), slots: slotsJSON(slot(0, false, 1, 100, 100, 0, 5, false))}
	srv := httptest.NewServer(m)
	defer srv.Close()
	s := newStepper(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	s.c.more = func() []string { return []string{srv.URL} }
	if h := s.step(8); !strings.Contains(h, "IDLE") || s.c.base != srv.URL {
		t.Errorf("discovered address not used (base %s):\n%s", s.c.base, h)
	}
	st := &state{sys: sysState{Pid: 7, Proc: "llama-server", Listen: []string{"0.0.0.0:8091", "[::]:8092", "192.168.1.5:9000"}}}
	if got := strings.Join(st.listenBases(), " "); got != "http://127.0.0.1:8091 http://127.0.0.1:8092 http://192.168.1.5:9000" {
		t.Errorf("listenBases %s", got)
	}
	st.sys.Guess = true
	if st.listenBases() != nil {
		t.Error("a guessed process's ports used")
	}
}

// The server restarts with another model (loading, then idle) while llm-top runs: the new model's slot is looked at
// once, so CONTEXT shows no leftover numbers from the old one.
func TestRestartRereadsSlots(t *testing.T) {
	m := &mock{metrics: metricsText(0), slots: slotsJSON(slotP(65536, slot(0, false, 9, 30000, 30000, 0, 500, false)))}
	srv := httptest.NewServer(m)
	defer srv.Close()
	s := newStepper(t, srv.URL)
	s.step(3)
	if s.st.srv.Slot.NPrompt != 30000 {
		t.Fatalf("startup slot not read: %+v", s.st.srv.Slot)
	}
	m.mu.Lock()
	m.health, m.metrics, m.slots = 503, "", slotsJSON(slotP(262144, slot(0, false, -1, 0, 0, 0, 0, false)))
	m.mu.Unlock()
	s.step(3) // loading
	m.mu.Lock()
	m.health, m.metrics = 0, metricsText(0)
	m.mu.Unlock()
	s.step(4)
	h := frame(t, s.st, 120, 40)
	if !s.st.srv.HaveSlots || s.st.srv.Slot.NPrompt != 0 || !strings.Contains(h, "0 / 33k") {
		t.Errorf("after restart: haveSlots=%v slot %+v\n%s", s.st.srv.HaveSlots, s.st.srv.Slot, h)
	}
}

// ---------- speculative decoding ----------

// slotT is slot() with the request's speculative.types.
func slotT(types string, s string) string {
	return strings.Replace(s, `"next_token"`, fmt.Sprintf(`"params":{"speculative.types":%q},"next_token"`, types), 1)
}

// specMetrics is metricsText with the speculative decoding counters.
func specMetrics(processing int, acc, tok, drafts float64) string {
	return metricsText(processing) + fmt.Sprintf("llamacpp:spec_decode_num_accepted_tokens_total %g\n"+
		"llamacpp:spec_decode_num_draft_tokens_total %g\nllamacpp:spec_decode_num_drafts_total %g\n", acc, tok, drafts)
}

func TestDrafters(t *testing.T) {
	for _, c := range []struct {
		in, want string
		known    bool
	}{
		{"", "", false}, {"  ", "", false}, {"none", "", true}, {"none,none", "", true},
		{"none,ngram-mod", "ngram-mod", true}, {"ngram-mod", "ngram-mod", true}, {"draft-dflash", "dflash", true},
		{"draft-dflash,ngram-mod", "dflash+ngram", true}, {"none, draft-dflash , ngram-mod", "dflash+ngram", true},
		{"draft", "draft", true}, {"ngram-mod,ngram-simple", "ngram", true}, {"eagle3,ngram-map-k", "eagle3+ngram", true},
	} {
		if n, k := drafters(c.in); n != c.want || k != c.known {
			t.Errorf("drafters(%q) = %q %v, want %q %v", c.in, n, k, c.want, c.known)
		}
	}
}

// Busy with ngram-mod: the counters move by 60 accepted of 100 drafted in 2 drafts per poll, while since start
// it is 1540 of 10900 (14.1%) in 118 drafts (13.1 tok/draft). A restart (counters going down) drops the window.
func TestSpecRecentAcceptance(t *testing.T) {
	m := &mock{}
	srv := httptest.NewServer(m)
	defer srv.Close()
	s := newStepper(t, srv.URL)
	acc, tok, drafts := 1000.0, 10000.0, 100.0
	m.mu.Lock()
	m.slots = slotsJSON(slotT("none,ngram-mod", slot(0, true, 4, 9000, 9000, 8000, 50, false)))
	m.metrics = specMetrics(1, acc, tok, drafts)
	m.mu.Unlock()
	for i := 0; i < 10; i++ {
		s.step(1)
		acc, tok, drafts = acc+60, tok+100, drafts+2
		m.mu.Lock()
		m.metrics = specMetrics(1, acc, tok, drafts)
		m.mu.Unlock()
	}
	sp := &s.st.srv.Spec
	rate, per, ok := sp.recent()
	if !ok || len(sp.Samples) != 9 || rate != 0.6 || per != 30 {
		t.Fatalf("recent = %v %v %v over %d samples, want 0.6 30 over 9", rate, per, ok, len(sp.Samples))
	}
	for _, w := range []int{120, 90} {
		out := frame(t, s.st, w, 40)
		for _, want := range []string{"ngram-mod ", "60.0%", "· 14.1% all", "tok/draft", "30.0", "· 13.1 all"} {
			if !strings.Contains(out, want) {
				t.Errorf("%d cols: no %q in\n%s", w, want, out)
			}
		}
		if strings.Contains(out, "ngram accept") {
			t.Error("old label")
		}
	}
	m.mu.Lock()
	m.metrics = specMetrics(1, 5, 20, 1) // restarted
	m.mu.Unlock()
	s.step(1)
	if len(sp.Samples) != 0 {
		t.Errorf("window kept %d samples across a restart", len(sp.Samples))
	}
	m.mu.Lock()
	m.metrics = specMetrics(1, 5+30, 20+100, 1+4)
	m.slots = slotsJSON(slotT("draft-dflash,ngram-mod", slot(0, true, 5, 9000, 9000, 8000, 50, false)))
	m.mu.Unlock()
	s.step(1)
	if rate, per, _ := sp.recent(); rate != 0.3 || per != 7.5 {
		t.Errorf("after restart: %v %v, want 0.3 7.5", rate, per)
	}
	if out := frame(t, s.st, 120, 40); !strings.Contains(out, "dflash+ngram") || !strings.Contains(out, "30.0%") {
		t.Errorf("combined drafters:\n%s", out)
	}
}

// Rows only when the server drafts: never without the counters or with speculative.types "none"; without slot
// params a server whose counters moved is shown as "draft".
func TestSpecHidden(t *testing.T) {
	for _, c := range []struct {
		name    string
		m       *mock
		want    string // "" = no rows
		samples bool
	}{
		{"no counters", &mock{metrics: metricsText(1), slots: slotsJSON(slotT("none", slot(0, true, 1, 900, 900, 0, 9, false)))}, "", false},
		{"types none", &mock{metrics: specMetrics(1, 0, 0, 0), slots: slotsJSON(slotT("none", slot(0, true, 1, 900, 900, 0, 9, false)))}, "", false},
		{"no slots, zero", &mock{noSlots: true, metrics: specMetrics(1, 0, 0, 0)}, "", false},
		{"no slots, drafted", &mock{noSlots: true, metrics: specMetrics(1, 40, 100, 5)}, "draft ", false},
		{"ngram, nothing drafted yet", &mock{metrics: specMetrics(1, 0, 0, 0), slots: slotsJSON(slotT("none,ngram-mod", slot(0, true, 1, 900, 900, 0, 9, false)))}, "ngram-mod ", false},
	} {
		srv := httptest.NewServer(c.m)
		st := run(t, srv.URL, "", "", 6, nil)
		srv.Close()
		out := frame(t, st, 120, 40)
		frame(t, st, 90, 40)
		if got := strings.Contains(out, "tok/draft"); got != (c.want != "") || c.want != "" && !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q:\n%s", c.name, c.want, out)
		}
		if len(st.srv.Spec.Samples) > 0 != c.samples {
			t.Errorf("%s: %d samples", c.name, len(st.srv.Spec.Samples))
		}
	}
}

func TestSpecPos(t *testing.T) {
	mt := parseMetrics(strings.NewReader(readFixture(t, "metrics.txt")))
	pos := specPos(mt)
	if len(pos) != 64 || pos[0] != 699.0/745 || pos[63] != 143.0/745 {
		t.Errorf("per position: %d %v", len(pos), pos)
	}
	if specPos(map[string]float64{specPosKey + ".0": 3}) != nil {
		t.Error("per position without drafts")
	}
}
