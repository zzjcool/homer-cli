//go:build linux

package agentd

import (
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/zzjcool/homer-cli/internal/hub"
)

func collectPlatform() *hub.HostSnapshot {
	snap := &hub.HostSnapshot{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if text, err := os.ReadFile("/etc/os-release"); err == nil {
		snap.Distro = parseOSRelease(string(text))
	}
	if text, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		snap.Kernel = strings.TrimSpace(string(text))
	}
	if text, err := os.ReadFile("/proc/uptime"); err == nil {
		snap.UptimeSec = parseUptime(string(text))
	}
	if text, err := os.ReadFile("/proc/meminfo"); err == nil {
		snap.Memory, snap.Swap = parseMeminfo(string(text))
	}
	if text, err := os.ReadFile("/proc/loadavg"); err == nil {
		if load, ok := parseLoadavg(string(text)); ok {
			snap.Load = &load
		}
	}
	if model := cpuModel(); model != "" {
		snap.CPU = &hub.HostCPU{Model: model}
	}
	snap.Disks = readDisks()
	return snap
}

func cpuModel() string {
	text, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	return parseCPUModel(string(text))
}

func readCPUSample() (cpuTimes, int, bool) {
	text, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, 0, false
	}
	return parseProcStat(string(text))
}

func readDisks() []hub.HostDisk {
	text, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	var root *hub.HostDisk
	var rest []hub.HostDisk
	seen := map[string]bool{}
	for _, mount := range parseMounts(string(text)) {
		if seen[mount] {
			continue
		}
		// Docker bind-mounts single files (/etc/hosts and friends) from the
		// host disk. Those are not filesystems to show.
		info, err := os.Stat(mount)
		if err != nil || !info.IsDir() {
			continue
		}
		total, used, ok := diskUsage(mount)
		if !ok || total == 0 {
			continue
		}
		seen[mount] = true
		disk := hub.HostDisk{Mount: mount, Total: total, Used: used}
		if mount == "/" {
			copy := disk
			root = &copy
			continue
		}
		rest = append(rest, disk)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Total > rest[j].Total })
	var disks []hub.HostDisk
	if root != nil {
		disks = append(disks, *root)
	}
	disks = append(disks, rest...)
	if len(disks) > 6 {
		disks = disks[:6]
	}
	return disks
}

func diskUsage(path string) (total, used uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Bsize <= 0 {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	total = uint64(st.Blocks) * bsize
	avail := uint64(st.Bavail) * bsize
	if total == 0 {
		return 0, 0, false
	}
	if avail > total {
		avail = total
	}
	return total, total - avail, true
}
