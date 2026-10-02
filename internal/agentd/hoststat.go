package agentd

import (
	"fmt"
	"math"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
)

// cpuTimes is one aggregate sample from the kernel's CPU counters.
type cpuTimes struct {
	idle  uint64
	total uint64
}

// hostCollector keeps the previous CPU sample so utilization is a delta
// across heartbeats instead of a blocking sample on every poll.
type hostCollector struct {
	mu   sync.Mutex
	prev cpuTimes
	have bool
}

func (c *hostCollector) snapshot() *hub.HostSnapshot {
	if c == nil {
		c = &hostCollector{}
	}
	snap := collectPlatform()
	if snap == nil {
		snap = &hub.HostSnapshot{}
	}
	if snap.OS == "" {
		snap.OS = runtime.GOOS
	}
	if snap.Arch == "" {
		snap.Arch = runtime.GOARCH
	}
	snap.Nets = readNets()

	sample, cores, ok := readCPUSample()
	if !ok {
		if snap.CPU == nil && runtime.NumCPU() > 0 {
			snap.CPU = &hub.HostCPU{Cores: runtime.NumCPU()}
		}
		return snap
	}
	if snap.CPU == nil {
		snap.CPU = &hub.HostCPU{}
	}
	if cores > 0 {
		snap.CPU.Cores = cores
	} else if snap.CPU.Cores == 0 {
		snap.CPU.Cores = runtime.NumCPU()
	}
	if model := cpuModel(); model != "" && snap.CPU.Model == "" {
		snap.CPU.Model = model
	}

	c.mu.Lock()
	prev, have := c.prev, c.have
	c.mu.Unlock()
	if !have {
		time.Sleep(200 * time.Millisecond)
		if next, _, ok2 := readCPUSample(); ok2 {
			prev, sample, have = sample, next, true
		}
	}
	if have {
		if pct, ok := cpuPercent(prev, sample); ok {
			pct = math.Round(pct*10) / 10
			snap.CPU.Usage = &pct
		}
	}
	c.mu.Lock()
	c.prev = sample
	c.have = true
	c.mu.Unlock()
	return snap
}

func cpuPercent(prev, next cpuTimes) (float64, bool) {
	if next.total <= prev.total {
		return 0, false
	}
	total := next.total - prev.total
	idle := next.idle - prev.idle
	if idle > total {
		idle = total
	}
	return 100 * float64(total-idle) / float64(total), true
}

func parseProcStat(text string) (cpuTimes, int, bool) {
	var sample cpuTimes
	var cores int
	var ok bool
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			var nums []uint64
			valid := true
			for _, field := range fields[1:] {
				n, err := strconv.ParseUint(field, 10, 64)
				if err != nil {
					valid = false
					break
				}
				nums = append(nums, n)
			}
			if !valid || len(nums) < 4 {
				continue
			}
			var total uint64
			for _, n := range nums {
				total += n
			}
			idle := nums[3]
			if len(nums) > 4 {
				idle += nums[4] // iowait counts as not busy
			}
			sample = cpuTimes{idle: idle, total: total}
			ok = true
			continue
		}
		if len(line) > 3 && strings.HasPrefix(line, "cpu") && line[3] >= '0' && line[3] <= '9' {
			cores++
		}
	}
	return sample, cores, ok
}

func parseMeminfo(text string) (mem, swap *hub.HostMemory) {
	vals := map[string]uint64{}
	found := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) >= 3 && fields[2] == "kB" {
			n *= 1024
		}
		key := strings.TrimSuffix(fields[0], ":")
		vals[key] = n
		found[key] = true
	}
	if found["MemTotal"] && vals["MemTotal"] > 0 {
		total := vals["MemTotal"]
		var avail uint64
		if found["MemAvailable"] {
			avail = vals["MemAvailable"]
		} else {
			avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
		}
		if avail > total {
			avail = total
		}
		mem = &hub.HostMemory{Total: total, Used: total - avail}
	}
	if found["SwapTotal"] && vals["SwapTotal"] > 0 {
		total := vals["SwapTotal"]
		free := vals["SwapFree"]
		if free > total {
			free = total
		}
		swap = &hub.HostMemory{Total: total, Used: total - free}
	}
	return mem, swap
}

func parseLoadavg(text string) (hub.HostLoad, bool) {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return hub.HostLoad{}, false
	}
	one, err1 := strconv.ParseFloat(fields[0], 64)
	five, err5 := strconv.ParseFloat(fields[1], 64)
	fifteen, err15 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err5 != nil || err15 != nil {
		return hub.HostLoad{}, false
	}
	return hub.HostLoad{One: one, Five: five, Fifteen: fifteen}, true
}

func parseUptime(text string) int64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return int64(secs)
}

func parseOSRelease(text string) string {
	var name, pretty string
	for _, line := range strings.Split(text, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "PRETTY_NAME":
			pretty = val
		case "NAME":
			name = val
		}
	}
	if pretty != "" {
		return pretty
	}
	return name
}

func parseCPUModel(text string) string {
	for _, line := range strings.Split(text, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// realFilesystem reports whether a mount's type is a disk the console
// should show. Virtual and pseudo filesystems are omitted. Overlay is the
// container root, so only "/" counts; nested overlay mounts stay hidden.
func realFilesystem(fstype, mount string) bool {
	if fstype == "overlay" {
		return mount == "/"
	}
	switch fstype {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "bcachefs", "f2fs",
		"ntfs", "ntfs3", "vfat", "exfat", "fuseblk", "ufs", "apfs":
		return true
	default:
		return false
	}
}

func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		oct := s[i+1 : i+4]
		n, err := strconv.ParseUint(oct, 8, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String()
}

// parseMounts returns real filesystem mountpoints, with "/" first.
func parseMounts(text string) []string {
	seen := map[string]bool{}
	var rest []string
	root := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !realFilesystem(fields[2], unescapeMount(fields[1])) {
			continue
		}
		mount := unescapeMount(fields[1])
		if mount == "" || seen[mount] {
			continue
		}
		seen[mount] = true
		if mount == "/" {
			root = true
			continue
		}
		rest = append(rest, mount)
	}
	sort.Strings(rest)
	if root {
		return append([]string{"/"}, rest...)
	}
	return rest
}

func readNets() []hub.HostNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []hub.HostNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Name == "lo" {
			continue
		}
		item := hub.HostNet{
			Name: iface.Name,
			Up:   iface.Flags&net.FlagUp != 0,
		}
		if mac := iface.HardwareAddr.String(); mac != "" && mac != "00:00:00:00:00:00" {
			item.MAC = mac
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet == nil || !ipnet.IP.IsGlobalUnicast() {
				continue
			}
			ip := ipnet.IP
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			ones, bits := ipnet.Mask.Size()
			if bits == 0 {
				item.Addrs = append(item.Addrs, ip.String())
				continue
			}
			item.Addrs = append(item.Addrs, fmt.Sprintf("%s/%d", ip.String(), ones))
		}
		sort.SliceStable(item.Addrs, func(i, j int) bool {
			iv6 := strings.Contains(item.Addrs[i], ":")
			jv6 := strings.Contains(item.Addrs[j], ":")
			if iv6 != jv6 {
				return !iv6
			}
			return item.Addrs[i] < item.Addrs[j]
		})
		if len(item.Addrs) > 8 {
			item.Addrs = item.Addrs[:8]
		}
		if !item.Up && len(item.Addrs) == 0 {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := netRank(out[i]), netRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func netRank(n hub.HostNet) int {
	virtual := virtualNIC(n.Name)
	switch {
	case n.Up && len(n.Addrs) > 0 && !virtual:
		return 0
	case n.Up && len(n.Addrs) > 0:
		return 1
	case n.Up && !virtual:
		return 2
	case n.Up:
		return 3
	default:
		return 4
	}
}

func virtualNIC(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{
		"veth", "docker", "br-", "virbr", "flannel", "cni", "cali",
		"lxc", "tunl", "kube", "podman", "vnet", "tap",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
