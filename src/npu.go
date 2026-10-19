// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// NPU counters (Windows), read in the same 5 s machine probe:
//
//   - Windows' GPU performance counters, from the PowerShell probe in sys.go. An NPU is a compute-only adapter
//     (PnP class ComputeAccelerator), so it has engine counters like a GPU: its adapter is the one whose engines are
//     all of type "Compute" (an iGPU has "Compute 0", "3D", "Copy", ...). Utilization is the sum over that adapter's
//     engine instances, memory its adapter memory (shared + dedicated), and every process with an engine instance on
//     it is listed with its share. This works for any vendor's NPU that exposes these counters.
//   - AMD's xrt-smi (`examine -r aie-partitions`), when it is installed: the hardware contexts with their process,
//     status and submission/completion counters (commands per second are deltas between probes) and the memory the
//     partitions hold. Its JSON output needs a file, so the text table is parsed from stdout instead and nothing is
//     written. It is only run when the probe found an NPU.
//
// No power or temperature: the drivers expose neither. No clock either: AMD's is only readable through XRT's C++ API,
// which needs cgo, and this is a static binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type npuState struct {
	Name    string  // PnP device name; "" = no NPU
	Util    float64 // %, -1 = no engine counters
	MemGB   float64 // adapter memory in use, -1 = unknown
	Procs   []npuProc
	Smi     smiState
	CmdRate float64 // completions per second over all hardware contexts, -1 = unknown
}

type npuProc struct {
	Pid  int
	Name string
	Util float64
}

type smiState struct {
	OK       bool // xrt-smi ran and its report parsed
	MemMB    float64
	Contexts []npuCtx
}

type npuCtx struct {
	Pid, ID         int
	Proc, Status    string
	Submitted, Done float64
}

// xrtSmiPath is AMD's xrt-smi from the NPU driver, else one on PATH; "" = none.
func xrtSmiPath() string {
	if r := os.Getenv("SystemRoot"); r != "" {
		p := filepath.Join(r, "System32", "AMD", "xrt-smi.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("xrt-smi"); err == nil {
		return p
	}
	return ""
}

func probeXrtSmi() smiState {
	p := xrtSmiPath()
	if p == "" {
		return smiState{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "examine", "-r", "aie-partitions").Output()
	if err != nil {
		return smiState{}
	}
	return parseXrtSmi(string(out))
}

var smiMem = regexp.MustCompile(`Total Memory Usage:\s*([\d.]+)\s*MB`)

// parseXrtSmi reads `xrt-smi examine -r aie-partitions`. Each hardware context is a group of table rows between
// separators: row 1 PID | Ctx ID | Submissions | ..., row 2 Process Name | Status | Completions | ...
func parseXrtSmi(out string) smiState {
	out = strings.ReplaceAll(out, "\r", "")
	if !strings.Contains(out, "AIE Partitions") {
		return smiState{}
	}
	s := smiState{OK: true}
	if m := smiMem.FindStringSubmatch(out); m != nil {
		s.MemMB, _ = strconv.ParseFloat(m[1], 64)
	}
	var group [][]string
	inTable := false
	flush := func() {
		if len(group) >= 2 && len(group[0]) >= 3 && len(group[1]) >= 3 {
			pid, e1 := strconv.Atoi(group[0][0])
			id, e2 := strconv.Atoi(group[0][1])
			sub, e3 := strconv.ParseFloat(group[0][2], 64)
			done, e4 := strconv.ParseFloat(group[1][2], 64)
			if e1 == nil && e2 == nil && e3 == nil && e4 == nil {
				s.Contexts = append(s.Contexts, npuCtx{pid, id, group[1][0], group[1][1], sub, done})
			}
		}
		group = nil
	}
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "|==="):
			inTable, group = true, nil
		case strings.HasPrefix(t, "|---"):
			flush()
		case inTable && strings.HasPrefix(t, "|"):
			var f []string
			for _, c := range strings.Split(strings.Trim(t, "|"), "|") {
				f = append(f, strings.TrimSpace(c))
			}
			group = append(group, f)
		case inTable && t == "":
			flush()
			inTable = false
		}
	}
	flush()
	return s
}

// npuRate sums the completions since the previous probe over the contexts present in both.
func npuRate(cur, prev smiState, dt float64) float64 {
	if !cur.OK || !prev.OK || dt <= 0 {
		return -1
	}
	was := map[[2]int]float64{}
	for _, c := range prev.Contexts {
		was[[2]int{c.Pid, c.ID}] = c.Done
	}
	r := 0.0
	for _, c := range cur.Contexts {
		if p, ok := was[[2]int{c.Pid, c.ID}]; ok && c.Done >= p {
			r += (c.Done - p) / dt
		}
	}
	return r
}

// npuLine is the muted line under the NPU gauge: hardware contexts (xrt-smi) or the processes using it. The device
// name is left out: Windows names every NPU "NPU Compute Accelerator Device".
func npuLine(n npuState) string {
	var parts []string
	var names []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	if n.Smi.OK {
		active := 0
		for _, c := range n.Smi.Contexts {
			if strings.EqualFold(c.Status, "Active") {
				active++
			}
		}
		switch len(n.Smi.Contexts) {
		case 0:
			parts = append(parts, "no hardware contexts")
		case 1:
			parts = append(parts, fmt.Sprintf("1 context (%d active)", active))
		default:
			parts = append(parts, fmt.Sprintf("%d contexts (%d active)", len(n.Smi.Contexts), active))
		}
		if n.CmdRate >= 0 && len(n.Smi.Contexts) > 0 {
			parts = append(parts, fmt.Sprintf("%s cmds/s", thousands(int(n.CmdRate+0.5))))
		}
		cs := append([]npuCtx(nil), n.Smi.Contexts...)
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].Done > cs[j].Done })
		for _, c := range cs {
			add(fmt.Sprintf("%s %d", strings.TrimSuffix(c.Proc, ".exe"), c.Pid))
		}
	} else {
		for _, p := range n.Procs {
			if p.Util > 0 {
				add(fmt.Sprintf("%s %d", p.Name, p.Pid))
			}
		}
		if len(names) == 0 {
			parts = append(parts, "idle")
		}
	}
	if len(names) > 0 {
		parts = append(parts, strings.Join(names, ", "))
	}
	return strings.Join(parts, " · ")
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
