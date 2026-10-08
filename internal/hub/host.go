package hub

import (
	"math"
	"strings"
	"unicode/utf8"
)

// HostSnapshot is the machine resource report an agent uploads with
// hello and heartbeats. The console renders it on the machine card.
// Every field is best-effort: a platform that cannot read one source omits it.
type HostSnapshot struct {
	OS        string      `json:"os,omitempty"`
	Arch      string      `json:"arch,omitempty"`
	Distro    string      `json:"distro,omitempty"`
	Kernel    string      `json:"kernel,omitempty"`
	UptimeSec int64       `json:"uptimeSec,omitempty"`
	CPU       *HostCPU    `json:"cpu,omitempty"`
	Memory    *HostMemory `json:"memory,omitempty"`
	Swap      *HostMemory `json:"swap,omitempty"`
	Load      *HostLoad   `json:"load,omitempty"`
	Disks     []HostDisk  `json:"disks,omitempty"`
	Nets      []HostNet   `json:"nets,omitempty"`
}

// HostCPU is logical CPU count, a model string, and utilization over the
// interval since the previous sample (0–100). Usage is omitted until the
// agent has two samples.
type HostCPU struct {
	Cores int      `json:"cores,omitempty"`
	Model string   `json:"model,omitempty"`
	Usage *float64 `json:"usage,omitempty"`
}

// HostMemory is a byte count. Used is total minus available (not just free).
type HostMemory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// HostLoad is the 1, 5, and 15 minute load average.
type HostLoad struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// HostDisk is one mounted filesystem.
type HostDisk struct {
	Mount string `json:"mount"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// HostNet is one non-loopback interface and the addresses the agent could see.
type HostNet struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
	Up    bool     `json:"up"`
}

// normalizeHost copies a reported snapshot into registry-owned memory and
// drops empty or absurd values. A nil result means the payload carried
// nothing worth showing.
func normalizeHost(in *HostSnapshot) *HostSnapshot {
	if in == nil {
		return nil
	}
	out := HostSnapshot{
		OS:        clipText(in.OS, 32),
		Arch:      clipText(in.Arch, 32),
		Distro:    clipText(in.Distro, 80),
		Kernel:    clipText(in.Kernel, 80),
		UptimeSec: in.UptimeSec,
	}
	if out.UptimeSec < 0 {
		out.UptimeSec = 0
	}
	if in.CPU != nil {
		cpu := HostCPU{
			Cores: in.CPU.Cores,
			Model: clipText(in.CPU.Model, 80),
		}
		if cpu.Cores < 0 {
			cpu.Cores = 0
		}
		if cpu.Cores > 4096 {
			cpu.Cores = 4096
		}
		if usage, ok := cleanPercent(in.CPU.Usage); ok {
			cpu.Usage = &usage
		}
		if cpu.Cores > 0 || cpu.Model != "" || cpu.Usage != nil {
			out.CPU = &cpu
		}
	}
	out.Memory = normalizeMemory(in.Memory)
	out.Swap = normalizeMemory(in.Swap)
	if in.Load != nil {
		out.Load = &HostLoad{
			One:     cleanLoad(in.Load.One),
			Five:    cleanLoad(in.Load.Five),
			Fifteen: cleanLoad(in.Load.Fifteen),
		}
	}
	for _, disk := range in.Disks {
		if len(out.Disks) >= 6 {
			break
		}
		mount := clipText(disk.Mount, 128)
		if mount == "" || disk.Total == 0 {
			continue
		}
		used := disk.Used
		if used > disk.Total {
			used = disk.Total
		}
		out.Disks = append(out.Disks, HostDisk{Mount: mount, Total: disk.Total, Used: used})
	}
	for _, net := range in.Nets {
		if len(out.Nets) >= 12 {
			break
		}
		name := clipText(net.Name, 32)
		if name == "" {
			continue
		}
		item := HostNet{Name: name, MAC: clipText(net.MAC, 32), Up: net.Up}
		for _, addr := range net.Addrs {
			if len(item.Addrs) >= 8 {
				break
			}
			addr = clipText(addr, 80)
			if !plausibleAddr(addr) {
				continue
			}
			item.Addrs = append(item.Addrs, addr)
		}
		out.Nets = append(out.Nets, item)
	}
	if out.OS == "" && out.Arch == "" && out.Distro == "" && out.Kernel == "" &&
		out.UptimeSec == 0 && out.CPU == nil && out.Memory == nil && out.Swap == nil &&
		out.Load == nil && len(out.Disks) == 0 && len(out.Nets) == 0 {
		return nil
	}
	return &out
}

func normalizeMemory(in *HostMemory) *HostMemory {
	if in == nil || in.Total == 0 {
		return nil
	}
	used := in.Used
	if used > in.Total {
		used = in.Total
	}
	return &HostMemory{Total: in.Total, Used: used}
}

func cleanPercent(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, false
	}
	pct := *v
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return math.Round(pct*10) / 10, true
}

func cleanLoad(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 10000 {
		return 10000
	}
	return math.Round(v*100) / 100
}

func clipText(s string, maxRunes int) string {
	s = strings.TrimSpace(stripControls(s))
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return s
}

// stripControls drops C0 controls and CSI color sequences an agent might
// have copied out of a terminal.
func stripControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
				if i < len(s) {
					i++
				}
				continue
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r < 0x20 || r == 0x7f {
			i += size
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func plausibleAddr(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":")
}
