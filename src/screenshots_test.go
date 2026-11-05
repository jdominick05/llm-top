// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

// README images: real render() output of made-up state, converted from ANSI to SVG. Skipped unless asked for:
//
//	LLMTOP_SHOTS=docs go test -run TestScreenshots ./src
//
// A relative LLMTOP_SHOTS is taken from the repository root (tests run in src/). Writes docs/banner.svg, docs/demo.svg (animated), docs/lmstudio.svg and docs/narrow.svg. No real sessions,
// paths or numbers go in; everything below is invented.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- demo state ----------

func demoSession(t *testing.T, now time.Time) *session {
	at := func(s int) string { return now.Add(time.Duration(s) * time.Second).UTC().Format(time.RFC3339Nano) }
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	add(`{"type":"user","timestamp":%q,"cwd":"/home/dev/src/rocket-api","message":{"parts":[{"text":"add rate limiting to the upload endpoint and write tests"}]}}`, at(-300))
	prompt := 9000
	for i, c := range []struct{ tool, arg, key string }{
		{"glob", "src/**/*.go", "pattern"}, {"read_file", "/home/dev/src/rocket-api/src/upload.go", "absolute_path"},
		{"grep_search", "func.*Limiter", "pattern"}, {"read_file", "/home/dev/src/rocket-api/src/middleware.go", "absolute_path"},
		{"edit", "/home/dev/src/rocket-api/src/middleware.go", "file_path"}, {"write_file", "/home/dev/src/rocket-api/src/ratelimit.go", "file_path"},
		{"run_shell_command", "Run the upload tests", "description"}, {"edit", "/home/dev/src/rocket-api/src/upload.go", "file_path"},
		{"run_shell_command", "Run the full test suite", "description"}, {"mcp__github__create_pull_request", "Rate limit uploads", "title"},
	} {
		s := -280 + i*24
		prompt += 2600 + i*900
		if i == 5 {
			add(`{"type":"system","subtype":"chat_compression","timestamp":%q,"systemPayload":{"compressedHistory":[{"parts":[{"text":%q}]}]}}`, at(s-2), strings.Repeat("summary ", 9000))
			prompt = 41000
		}
		add(`{"type":"assistant","timestamp":%q,"message":{"parts":[{"text":%q,"thought":true},{"functionCall":{"id":"c%d","name":%q,"args":{%q:%q}}}]},"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":180,"cachedContentTokenCount":%d},"executionContext":{"modelId":"qwen3-coder"}}`,
			at(s), strings.Repeat("think ", 300), i, c.tool, c.key, c.arg, prompt, prompt-2400)
		res := `{"output":"` + strings.Repeat("x", 14000+i*5000) + `"}`
		if i == 6 {
			res = `{"error":"exit status 1"}`
		}
		add(`{"type":"tool_result","timestamp":%q,"message":{"parts":[{"functionResponse":{"id":"c%d","response":%s}}]}}`, at(s+2+i%3*3), i, res)
	}
	add(`{"type":"assistant","timestamp":%q,"message":{"parts":[{"text":"Added a token-bucket limiter (20 req/s per client) to /upload with tests; all 48 tests pass. PR #42 is open."}]},"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":90,"cachedContentTokenCount":%d}}`, at(-20), prompt+3000, prompt)
	add(`{"type":"user","timestamp":%q,"message":{"parts":[{"text":"nice, now add metrics for rejected requests"}]}}`, at(-8))
	add(`{"type":"assistant","timestamp":%q,"message":{"parts":[{"functionCall":{"id":"c99","name":"read_file","args":{"absolute_path":"/home/dev/src/rocket-api/src/metrics.go"}}}]},"usageMetadata":{"promptTokenCount":%d,"cachedContentTokenCount":%d}}`, at(-4), prompt+3600, prompt+3000)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, p, strings.Join(lines, "\n")+"\n")
	s := newSession(p, "rocket-api")
	s.tail()
	s.Mod = now.Add(-4 * time.Second)
	return s
}

func demoState(t *testing.T) *state {
	now := time.Now()
	cfg := defaultConfig
	cfg.path = "~/.config/llm-top/llm-top.json"
	st := &state{cfg: cfg, bases: []string{"http://127.0.0.1:8080"}, alias: "qwen3-coder", model: "Qwen3-Coder-30B-A3B-Q4_K_M", known: map[string]*session{}}
	s1 := demoSession(t, now)
	s2 := newSession("b", "infra-scripts")
	s2.model, s2.turns, s2.tools, s2.compactions, s2.Mod = "qwen3-coder", 14, 61, 2, now.Add(-38*time.Minute)
	s3 := newSession("c", "notes")
	s3.model, s3.turns, s3.tools, s3.Mod = "qwen3-coder", 3, 7, now.Add(-2*time.Hour-11*time.Minute)
	st.sessions = []*session{s1, s2, s3}
	st.sys = sysState{OK: true, VramGB: 21.6, VramTot: 24, RamFree: 38.2, RamTot: 64, Pid: 4242, Proc: "llama-server",
		SrvWS: 1.4, ProcVram: 20.9, Spill: 0.31, ProcSwap: -1, GPUUtil: 97, SwapGB: 0.4, Cores: 0.8}
	st.sysTried = true
	st.sav = savingsState{Ready: true, First: now.AddDate(0, 0, -23), Days: 23,
		All:   tokTotals{Reqs: 18340, Prompt: 1_412_000_000, Cached: 1_351_000_000, Out: 21_700_000},
		Today: tokTotals{Reqs: 912, Prompt: 61_300_000, Cached: 58_900_000, Out: 1_040_000}}
	st.srv = serverState{Kind: "llama.cpp", Base: st.bases[0], Polled: true, Up: true, HaveSlots: true, NSlots: 1,
		Props: propsJSON{Alias: "qwen3-coder"},
		Metrics: map[string]float64{"prompt_tokens_total": 9_820_000, "prompt_tokens_cached_total": 131_400_000,
			"prompt_seconds_total": 3950, "tokens_predicted_total": 1_630_000, "tokens_predicted_seconds_total": 21_400,
			"spec_decode_num_draft_tokens_total": 920_000, "spec_decode_num_accepted_tokens_total": 401_000, "spec_decode_num_drafts_total": 160_000,
			"n_tokens_max": 118_000}}
	st.srv.Slot.Params.SpecTypes = "none,ngram-mod"
	for i := 0; i < 64; i++ {
		st.srv.Metrics[specPosKey+"."+strconv.Itoa(i)] = 160_000 * 0.9 * math.Pow(0.93, float64(i))
	}
	// some history in the sparklines
	for i := 0; i < 160; i++ {
		x := float64(i)
		st.srv.ReadRate.push(math.Max(0, 2600*math.Sin(x/7))*float64(i%23/18), 240)
		st.srv.GenRate.push(math.Max(0, 74+8*math.Sin(x/3))*float64(i%23/13%2), 240)
		if i%3 == 0 {
			st.srv.Spec.Samples = append(st.srv.Spec.Samples, specSample{acc: 30 + 25*math.Sin(x/9), tok: 64, drafts: 12})
		}
	}
	return st
}

func lmProps(nctx int) propsJSON {
	var p propsJSON
	p.Gen.NCtx = nctx
	return p
}

// ---------- animated SVG ----------

// svgFrames writes a terminal window; several frames play in a loop, each frameDur long. Lines shared by frames
// are stored once.
func svgFrames(frames []string, title string, frameDur float64) string {
	var parsed [][][]cell
	cols, rows := 0, 0
	for _, f := range frames {
		p := parseANSI(f)
		parsed = append(parsed, p)
		rows = max(rows, len(p))
		for _, r := range p {
			cols = max(cols, len(r))
		}
	}
	W, H := padX*2+float64(cols)*cw, padTop+float64(rows)*chH+14
	var defs, body strings.Builder
	ids := map[string]string{}
	for fi, p := range parsed {
		var g strings.Builder
		for ri, row := range p {
			ln := svgLine(row)
			if ln == "" {
				continue
			}
			id, ok := ids[ln]
			if !ok {
				id = fmt.Sprintf("l%d", len(ids))
				ids[ln] = id
				fmt.Fprintf(&defs, `<g id="%s">%s</g>`+"\n", id, ln)
			}
			fmt.Fprintf(&g, `<use href="#%s" y="%.1f"/>`, id, float64(ri)*chH)
		}
		n := len(parsed)
		if n == 1 {
			fmt.Fprintf(&body, "<g>%s</g>\n", g.String())
			continue
		}
		total := frameDur * float64(n)
		var vals, keys string
		switch {
		case fi == 0:
			vals, keys = "1;0", fmt.Sprintf("0;%.4f", 1/float64(n))
		case fi == n-1:
			vals, keys = "0;1", fmt.Sprintf("0;%.4f", float64(fi)/float64(n))
		default:
			vals, keys = "0;1;0", fmt.Sprintf("0;%.4f;%.4f", float64(fi)/float64(n), float64(fi+1)/float64(n))
		}
		init := "0"
		if fi == 0 {
			init = "1"
		}
		fmt.Fprintf(&body, `<g opacity="%s"><animate attributeName="opacity" values="%s" keyTimes="%s" dur="%.2fs" calcMode="discrete" repeatCount="indefinite"/>%s</g>`+"\n",
			init, vals, keys, total, g.String())
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="%s" font-size="14" xml:space="preserve">
<rect width="100%%" height="100%%" rx="10" fill="%s"/>
<circle cx="20" cy="18" r="6" fill="#bf616a"/><circle cx="40" cy="18" r="6" fill="#ebcb8b"/><circle cx="60" cy="18" r="6" fill="#a3be8c"/>
<text x="%.0f" y="23" fill="#6b7489" text-anchor="middle" font-size="13">%s</text>
<defs>
%s</defs>
<g transform="translate(%.0f %.0f)">
%s</g>
</svg>
`, W, H, W, H, svgFont, bgColor, W/2, xmlEsc(title), defs.String(), padX, padTop, body.String())
}

// ---------- banner ----------

var bannerArt = []string{
	`██╗     ██╗     ███╗   ███╗      ████████╗ ██████╗ ██████╗ `,
	`██║     ██║     ████╗ ████║      ╚══██╔══╝██╔═══██╗██╔══██╗`,
	`██║     ██║     ██╔████╔██║█████╗   ██║   ██║   ██║██████╔╝`,
	`██║     ██║     ██║╚██╔╝██║╚════╝   ██║   ██║   ██║██╔═══╝ `,
	`███████╗███████╗██║ ╚═╝ ██║         ██║   ╚██████╔╝██║     `,
	`╚══════╝╚══════╝╚═╝     ╚═╝         ╚═╝    ╚═════╝ ╚═╝     `,
}

// bannerSVG colors the art left to right from the accent to purple, shadows (box lines) dimmer. Blocks and
// shadows get placeholder colors that become SVG gradients, so neighbouring blocks merge into one shape.
func bannerSVG() string {
	var b strings.Builder
	n := len([]rune(bannerArt[0]))
	for _, l := range bannerArt {
		for _, r := range []rune(l) {
			if r == '█' {
				b.WriteString("\x1b[38;2;1;1;1m█")
			} else {
				fmt.Fprintf(&b, "\x1b[38;2;2;2;2m%c", r)
			}
		}
		b.WriteString("\x1b[m\n")
	}
	b.WriteString("\n" + fmt.Sprintf("\x1b[38;2;107;116;137m%s\x1b[m", centered("top for your local LLM: llama.cpp · LM Studio · Qwen Code", n)))
	rows := parseANSI(b.String())
	W, H := float64(n)*cw+40, float64(len(rows))*chH+36
	var body strings.Builder
	for ri, row := range rows {
		fmt.Fprintf(&body, `<g transform="translate(20 %.1f)">%s</g>`+"\n", 18+float64(ri)*chH, svgLine(row))
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="%s" font-size="14" xml:space="preserve">
<defs>
<linearGradient id="gb" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="%.0f" y2="0"><stop offset="0" stop-color="#88c0d0"/><stop offset=".55" stop-color="#81a1c1"/><stop offset="1" stop-color="#b48ead"/></linearGradient>
<linearGradient id="gs" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="%.0f" y2="0"><stop offset="0" stop-color="#4c6a78"/><stop offset="1" stop-color="#5e4a62"/></linearGradient>
</defs>
<rect width="100%%" height="100%%" rx="10" fill="%s"/>
%s</svg>
`, W, H, W, H, svgFont, float64(n)*cw, float64(n)*cw, bgColor,
		strings.NewReplacer(`fill="#010101"`, `fill="url(#gb)"`, `fill="#020202"`, `fill="url(#gs)"`).Replace(body.String()))
}

func centered(s string, w int) string {
	n := len([]rune(s))
	if n >= w {
		return s
	}
	return strings.Repeat(" ", (w-n)/2) + s
}

// ---------- the test ----------

func TestScreenshots(t *testing.T) {
	dir := os.Getenv("LLMTOP_SHOTS")
	if dir == "" {
		t.Skip("set LLMTOP_SHOTS=docs to regenerate the README images")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("..", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := version
	version = "0.1.0"
	defer func() { version = old }()
	save := func(name, svg string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(svg), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d KB", name, len(svg)/1024)
	}
	save("banner.svg", bannerSVG())

	// demo: a prompt is read, the answer written, then idle
	st := demoState(t)
	srv := &st.srv
	var frames []string
	const W, H = 132, 44
	prompt, cache := 61400, 52800
	step := func(i int) { // each frame is 0.45 s later in the request
		srv.busySince = time.Now().Add(-time.Duration(float64(i)*0.45*float64(time.Second)) - 300*time.Millisecond)
		frames = append(frames, model{st: st, w: W, h: H, frame: i * 3}.render())
	}
	for i := 0; i < 7; i++ { // reading
		done := min(prompt-cache, (i+1)*1400)
		srv.Slot = slotJSON{NCtx: 131072, Processing: true, Task: 7, NPrompt: prompt, NProcessed: done, NCache: cache}
		srv.Busy = 1
		srv.Decoded, srv.curRead, srv.curGen = 0, 2380+float64(i*37), 0
		srv.ReadRate.push(srv.curRead, 240)
		srv.GenRate.push(0, 240)
		st.sys.GPUUtil, st.sys.Cores = 98, 0.9
		step(i)
	}
	for i := 0; i < 9; i++ { // writing
		srv.Slot.NProcessed = prompt - cache
		srv.Decoded = 40 + i*38
		srv.curGen = 76.4 + float64(i%3)*1.3
		srv.curRead = 0
		srv.ReadRate.push(0, 240)
		srv.GenRate.push(srv.curGen, 240)
		st.sys.GPUUtil, st.sys.Cores = 94, 0.7
		step(7 + i)
	}
	srv.Slot.Processing, srv.Busy = false, 0
	srv.curGen = 0
	st.sys.GPUUtil, st.sys.Cores = 0, 0.02
	for i := 0; i < 4; i++ { // idle
		srv.ReadRate.push(0, 240)
		srv.GenRate.push(0, 240)
		step(16 + i)
	}
	save("demo.svg", svgFrames(frames, "llm-top — llama-server + Qwen Code", 0.45))

	// LM Studio with the model spilling out of VRAM
	st = demoState(t)
	st.alias, st.model, st.bases = "", "", []string{"http://127.0.0.1:1234"}
	st.srv = serverState{Kind: "lmstudio", Base: st.bases[0], Polled: true, Up: true, HaveSlots: true, NSlots: 1, Busy: 1,
		Model: "qwen/qwen3-32b", LMLoaded: 1, Props: lmProps(131072),
		LM:   lmState{Phase: "writing", Decoded: 412, GenRate: 9.6, Debug: true, Found: true, Prompt: 18400, ReadRate: 310},
		Slot: slotJSON{NCtx: 131072, Processing: true, NPrompt: 18400, NProcessed: 18400}, Decoded: 412, curGen: 9.6, busySince: time.Now().Add(-43 * time.Second)}
	for i := 0; i < 160; i++ {
		st.srv.GenRate.push(math.Max(0, 9+2*math.Sin(float64(i)/4))*float64(i/40%2), 240)
		st.srv.ReadRate.push(0, 240)
	}
	st.sys.Proc, st.sys.VramGB, st.sys.Spill, st.sys.GPUUtil, st.sys.Cores, st.sys.ProcVram = "LM Studio", 23.8, 3.4, 41, 3.1, 23.1
	save("lmstudio.svg", svgFrames([]string{model{st: st, w: W, h: 40}.render()}, "llm-top — LM Studio, VRAM full", 1))

	// narrow terminal: one column
	st = demoState(t)
	st.srv.Slot = slotJSON{NCtx: 131072, NPrompt: 64600}
	st.srv.Decoded = 412
	save("narrow.svg", svgFrames([]string{model{st: st, w: 92, h: 70}.render()}, "llm-top — 92 columns", 1))
}
