// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// Bottleneck: one line that says what limits the server right now, from the inference process's fallbacks.
//
// In order: paging (memory read back from disk), VRAM spilled into system RAM (on Windows the driver moves what
// doesn't fit into shared GPU memory, which runs at PCIe speed), the CPU doing the work while the GPU waits
// (layers or KV cache on the CPU), and otherwise GPU-bound, which is what a healthy server looks like. Idle, it
// flags a server that burns CPU without work (busy-wait polling).
package main

import (
	"fmt"
	"image/color"
	"strings"
)

const (
	spillWarnGB  = 0.75 // shared GPU memory of the process; ~0.35 GB is normal on Windows
	vramFullFrac = 0.9  // spill only counts while dedicated VRAM is this full
	dgpuMinGB    = 2    // below this the GPU is integrated: shared memory is its normal memory
	lowRAMGB     = 1.5  // Windows page-ins only mean paging when free RAM is this low
	swapReadBack = 50   // pages/s of swap read back (Linux) that make swapped-out memory a problem
	pageInWarn   = 500  // pages/s read back from disk (2 MB/s)
	cpuBoundCore = 1.5  // cores while the GPU is below gpuBusyPct
	gpuBusyPct   = 50
	idleBurnCore = 1.0
)

func bottleneck(busy bool, ss sysState, vramTot float64, ncpu int) (string, color.Color) {
	if !ss.OK {
		return "", cMuted
	}
	proc := ss.Proc
	if proc == "" {
		proc = "the server"
	}
	linux := ss.ProcSwap >= 0 // Linux reports the process's swap; Windows -1
	// Windows counts every hard fault, mmap'd model files included, so a model load reads as paging unless RAM is short
	paging := ss.PageIn >= pageInWarn && (linux || ss.RamTot <= 0 || ss.RamFree < lowRAMGB)
	switch {
	case paging:
		return fmt.Sprintf("paging: %.0f MB/s read back from disk (RAM short)", ss.PageIn*4096/1e6), cRed
	case ss.ProcSwap >= 0.1 && ss.PageIn >= swapReadBack:
		return fmt.Sprintf("paging: %.1f GB of %s is in swap", ss.ProcSwap, proc), cRed
	case ss.ProcSwap >= 0.1 && !busy:
		return fmt.Sprintf("%.1f GB of %s is in swap (not read back now)", ss.ProcSwap, proc), cOrange
	case ss.Spill >= spillWarnGB && vramFull(ss, vramTot):
		c := cOrange
		if busy {
			c = cRed
		}
		return fmt.Sprintf("VRAM full: %.1f GB spilled to system RAM (slow)", ss.Spill), c
	case !busy && ss.Cores >= idleBurnCore:
		return fmt.Sprintf("idle but burning %.1f cores (busy-wait? --poll 0)", ss.Cores), cYellow
	case !busy:
		if vramTot > 0 && ss.VramGB > 0 {
			return fmt.Sprintf("idle · no fallbacks · %.1f GB VRAM headroom", max(0, vramTot-ss.VramGB)), cMuted
		}
		return "idle · no fallbacks", cMuted
	case ss.GPUUtil >= 0 && ss.GPUUtil < gpuBusyPct && ss.Cores >= cpuBoundCore:
		return fmt.Sprintf("CPU-bound: %.1f cores, GPU %.0f%% (layers on the CPU?)", ss.Cores, ss.GPUUtil), cOrange
	case ss.GPUUtil >= gpuBusyPct:
		return fmt.Sprintf("GPU-bound (normal): GPU %.0f%%, no fallbacks", ss.GPUUtil), cGreen
	case ss.Cores >= cpuBoundCore && ss.GPUUtil < 0:
		return fmt.Sprintf("busy on %.1f cores, GPU load unknown", ss.Cores), cYellow
	}
	return "busy · no fallbacks", cGreen
}

// vramFull says whether spill means "did not fit": dedicated VRAM is (nearly) full. Integrated GPUs and unified
// memory use system RAM by design, so they never count as full.
func vramFull(ss sysState, vramTot float64) bool {
	if vramTot <= 0 || ss.VramGB <= 0 {
		return true // unknown: trust the spill
	}
	return vramTot >= dgpuMinGB && ss.VramGB >= vramFullFrac*vramTot
}

// fallbackRows are the THROUGHPUT lines for spill, paging and load, plus the verdict. off (serverState.offline)
// replaces the verdict: there is no model to judge.
func fallbackRows(busy bool, off string, ss sysState, vramTot float64, ncpu int) []string {
	if !ss.OK {
		return nil
	}
	gb := func(x float64, warn bool) string {
		if x < 0 {
			return fg(cMuted).Render("n/a")
		}
		s := fmt.Sprintf("%.2f GB", x)
		if warn {
			return fg(cOrange).Render(s)
		}
		return s
	}
	spill := "RAM " + gb(ss.Spill, ss.Spill >= spillWarnGB && vramFull(ss, vramTot))
	rate := fmt.Sprintf("@ %.1f MB/s", ss.PageIn*4096/1e6) // read back from disk
	if ss.PageIn >= pageInWarn {
		rate = fg(cRed).Render(rate)
	}
	page := fmt.Sprintf("pagefile %.1f GB %s", ss.SwapGB, rate)
	if ss.ProcSwap >= 0 { // Linux: what of the process is in swap
		page = "swap " + gb(ss.ProcSwap, ss.ProcSwap >= 0.1) + " " + rate
	}
	rows := []string{fmt.Sprintf("%-6s%s · %s", "spill", spill, page)}
	if ss.Pid == 0 {
		rows = append(rows, fmt.Sprintf("%-6s%s", "load", fg(cMuted).Render("no inference process found (server_process in the config)")))
	} else {
		gpu := fg(cMuted).Render("n/a")
		if ss.GPUUtil >= 0 {
			gpu = fmt.Sprintf("%.0f%%", ss.GPUUtil)
		}
		cpu := "…"
		if ss.Cores >= 0 {
			cpu = fmt.Sprintf("%.1f", ss.Cores)
		}
		name := ss.Proc
		if ss.Guess {
			name += "?"
		}
		rows = append(rows, fmt.Sprintf("%-6sGPU %s · CPU %s of %d cores · %s", "load", gpu, cpu, ncpu, fg(cMuted).Render(name)))
	}
	if off != "" {
		v := "▸ " + off
		if vramTot > 0 && ss.VramGB > 0 {
			v += fmt.Sprintf(" · %.1f GB VRAM free", max(0, vramTot-ss.VramGB))
		}
		return append(rows, fg(cMuted).Render(v))
	}
	if v, c := bottleneck(busy, ss, vramTot, ncpu); v != "" {
		rows = append(rows, fg(c).Render("▸ "+strings.TrimSpace(v)))
	}
	return rows
}
