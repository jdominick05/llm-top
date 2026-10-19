// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"os"
	"strings"
	"testing"
)

// testdata/xrt_smi_aie_partitions.txt has the table layout of a real xrt-smi 2.21 capture (Phoenix NPU, one process
// holding two hardware contexts); the second context's counters are edited.
func TestParseXrtSmi(t *testing.T) {
	b, err := os.ReadFile("testdata/xrt_smi_aie_partitions.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(b), strings.ReplaceAll(string(b), "\n", "\r\n")} {
		s := parseXrtSmi(out)
		if !s.OK || s.MemMB != 1306 || len(s.Contexts) != 2 {
			t.Fatalf("%+v", s)
		}
		c := s.Contexts[0]
		if c.Pid != 26016 || c.ID != 59 || c.Proc != "python.exe" || c.Status != "Active" || c.Submitted != 21263 || c.Done != 21262 {
			t.Errorf("context 0: %+v", c)
		}
		if c := s.Contexts[1]; c.ID != 60 || c.Status != "Idle" || c.Done != 1221 {
			t.Errorf("context 1: %+v", c)
		}
	}
	idle := "\r\n-----------------------------\r\n[003d:00:01.1] : NPU Phoenix\r\n-----------------------------\r\nAIE Partitions\r\n  No hardware contexts running on device\r\n\r\n"
	if s := parseXrtSmi(idle); !s.OK || len(s.Contexts) != 0 {
		t.Errorf("idle: %+v", s)
	}
	for _, bad := range []string{"", "ERROR: no device", "|====|\n|x|y|z|\n|a|b|c|\n|---|\n"} {
		if s := parseXrtSmi(bad); s.OK || len(s.Contexts) != 0 {
			t.Errorf("accepted %q: %+v", bad, s)
		}
	}
}

func TestNpuRate(t *testing.T) {
	prev := smiState{OK: true, Contexts: []npuCtx{{Pid: 1, ID: 59, Done: 1000}, {Pid: 1, ID: 60, Done: 50}}}
	cur := smiState{OK: true, Contexts: []npuCtx{{Pid: 1, ID: 59, Done: 6000}, {Pid: 2, ID: 61, Done: 9999}}}
	if r := npuRate(cur, prev, 5); r != 1000 { // context 60 left, 61 is new: only 59 counts
		t.Errorf("rate %v", r)
	}
	if r := npuRate(cur, smiState{}, 5); r != -1 {
		t.Errorf("no previous probe: %v", r)
	}
	cur.Contexts[0].Done = 10 // counters went down (a new context with a reused id)
	if r := npuRate(cur, prev, 5); r != 0 {
		t.Errorf("reset: %v", r)
	}
}

func TestParseWinProbeNPU(t *testing.T) {
	ss := parseWinProbe(winProbeOut)
	if ss.NPU.Name != "" {
		t.Errorf("no npu lines: %+v", ss.NPU)
	}
	ss = parseWinProbe(winProbeOut + "npu_name NPU Compute Accelerator Device\r\nnpu_util 37\r\nnpu_mem 1369440256\r\n" +
		"npu_proc 25548 0 hwinfo_npu_bridge\r\nnpu_proc 26016 37 python\r\nnpu_proc bad line\r\nnpu_proc 7 x y\r\n")
	n := ss.NPU
	if !ss.OK || ss.GPUUtil != 93 || ss.Pid != 18184 {
		t.Fatalf("npu lines broke the probe: %+v", ss)
	}
	if n.Name != "NPU Compute Accelerator Device" || n.Util != 37 || !near(n.MemGB, 1.275) || len(n.Procs) != 2 || n.Procs[0].Name != "python" {
		t.Errorf("%+v", n)
	}
	// an NPU without engine counters
	n = parseWinProbe("ram_total_kb 8000000\nnpu_name NPU\n").NPU
	if n.Name != "NPU" || n.Util != -1 || n.MemGB != -1 {
		t.Errorf("no counters: %+v", n)
	}
}

func TestNpuLine(t *testing.T) {
	n := npuState{Name: "NPU", Util: 0, MemGB: 0, CmdRate: -1, Procs: []npuProc{{25548, "hwinfo_npu_bridge", 0}}}
	if l := npuLine(n); l != "idle" {
		t.Errorf("%q", l)
	}
	n.Procs = append(n.Procs, npuProc{26016, "python", 40})
	if l := npuLine(n); l != "python 26016" {
		t.Errorf("%q", l)
	}
	b, _ := os.ReadFile("testdata/xrt_smi_aie_partitions.txt")
	n.Smi, n.CmdRate = parseXrtSmi(string(b)), 1234.4
	if l := npuLine(n); l != "2 contexts (1 active) · 1,234 cmds/s · python 26016" {
		t.Errorf("%q", l)
	}
	n.Smi = smiState{OK: true}
	if l := npuLine(n); l != "no hardware contexts" {
		t.Errorf("%q", l)
	}
}

func TestRenderNPU(t *testing.T) {
	st := demoState(t)
	b, _ := os.ReadFile("testdata/xrt_smi_aie_partitions.txt")
	st.sys.NPU = npuState{Name: "NPU Compute Accelerator Device", Util: 88, MemGB: 1.28, CmdRate: 2087, Smi: parseXrtSmi(string(b))}
	for w := 1; w <= 130; w++ {
		for _, h := range []int{5, 30, 60} {
			frame(t, st, w, h)
		}
	}
	if f := frame(t, st, 120, 60); !strings.Contains(f, "NPU") || !strings.Contains(f, "2,087 cmds/s") {
		t.Errorf("no NPU rows:\n%s", f)
	}
}
