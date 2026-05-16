package dock

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// System info exposed by GET /api/system/info — intentionally PUBLIC
// (no auth) so deploy tooling can probe a fresh box's identity, free
// space, and reachability before pushing.
//
// What's exposed:
//   - hostname, uname -a, OS / arch
//   - polar version (git sha + build time, injected via -ldflags)
//   - all IP addresses (loopback + private LAN included — deploy tools
//     need to know the address they're hitting)
//   - per-mount disk usage
//   - memory total / available
//   - cpu count + model
//   - OS uptime (seconds since boot)
//
// What's NOT exposed:
//   - env vars (could leak API keys)
//   - process list (could reveal LLM workers / secrets in argv)
//   - DB connection state, file paths beyond mount points, etc.
//
// Cache: 5s server-side. Public endpoints get scanned; we don't want
// each scrape to fork uname + df + sysctl.

type SystemHostInfo struct {
	Hostname      string                `json:"hostname"`
	OS            string                `json:"os"`
	Arch          string                `json:"arch"`
	Uname         string                `json:"uname,omitempty"`
	Version       SystemHostVersionInfo `json:"version"`
	UptimeSeconds int64                 `json:"uptime_seconds"`
	IPs           []SystemHostIPInfo    `json:"ip_addresses"`
	Disks         []SystemHostDiskInfo  `json:"disks"`
	Memory        SystemHostMemoryInfo  `json:"memory"`
	CPU           SystemHostCPUInfo     `json:"cpu"`
	// Runtime: live dock-process state. NOT cached — refreshed on
	// every call even when the rest of the snapshot is served from
	// cache, because goroutines / heap change second-to-second.
	Runtime   *SystemHostRuntimeInfo `json:"runtime,omitempty"`
	SampledAt time.Time              `json:"sampled_at"`
}

type SystemHostVersionInfo struct {
	GitSHA    string `json:"git_sha,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	GitDirty  bool   `json:"git_dirty,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Go        string `json:"go"`
	Module    string `json:"module,omitempty"`
}

// SystemHostRuntimeInfo — live dock process state. Used by ops to
// spot leaks (rising goroutines/heap) or DB pool exhaustion.
type SystemHostRuntimeInfo struct {
	ProcessUptimeSeconds int64                `json:"process_uptime_seconds"`
	Goroutines           int                  `json:"goroutines"`
	HeapAllocBytes       uint64               `json:"heap_alloc_bytes"`
	HeapSysBytes         uint64               `json:"heap_sys_bytes"`
	GCRuns               uint32               `json:"gc_runs"`
	DBPool               *SystemHostDBPoolInfo `json:"db_pool,omitempty"`
}

type SystemHostDBPoolInfo struct {
	Open      int   `json:"open"`
	InUse     int   `json:"in_use"`
	Idle      int   `json:"idle"`
	WaitCount int64 `json:"wait_count"`
}

type SystemHostIPInfo struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	Family    string `json:"family"` // ipv4 | ipv6
}

type SystemHostDiskInfo struct {
	Mount      string  `json:"mount"`
	Filesystem string  `json:"filesystem,omitempty"`
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	FreeBytes  uint64  `json:"free_bytes"`
	UsedPct    float64 `json:"used_pct"`
}

type SystemHostMemoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`
}

type SystemHostCPUInfo struct {
	Count int    `json:"count"`
	Model string `json:"model,omitempty"`
}

// Build-time injection via:
//   go build -ldflags '-X gin-auth-app/internal/app/dock.buildGitSHA=$(git rev-parse --short HEAD)
//                      -X gin-auth-app/internal/app/dock.buildBuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)'
var (
	buildGitSHA    = ""
	buildGitBranch = ""
	buildGitDirty  = "" // "true" = dirty working tree at build time
	buildBuildTime = ""

	// processStartTime is set at package init; runtime exposes
	// (now - processStartTime) as the dock's own uptime, which is
	// distinct from OS uptime.
	processStartTime = time.Now()
)

const sysInfoCacheTTL = 5 * time.Second

var (
	sysInfoCache   atomic.Pointer[SystemHostInfo]
	sysInfoCacheAt atomic.Int64 // unix nano of cache contents
	sysInfoMu      sync.Mutex   // serialize refreshes — one shell-out at a time
)

// gatherSystemInfoCached returns the cached snapshot if fresh, otherwise
// refreshes. Cheap to call from the HTTP handler. The Runtime sub-struct
// is always refreshed even on cache hit — process state changes
// second-to-second.
func (s *Server) gatherSystemInfoCached() *SystemHostInfo {
	var base *SystemHostInfo
	if cached := sysInfoCache.Load(); cached != nil {
		at := time.Unix(0, sysInfoCacheAt.Load())
		if time.Since(at) < sysInfoCacheTTL {
			base = cached
		}
	}
	if base == nil {
		sysInfoMu.Lock()
		defer sysInfoMu.Unlock()
		// Double-check under lock — another goroutine may have just refreshed.
		if cached := sysInfoCache.Load(); cached != nil {
			at := time.Unix(0, sysInfoCacheAt.Load())
			if time.Since(at) < sysInfoCacheTTL {
				base = cached
			}
		}
		if base == nil {
			base = gatherSystemInfo()
			sysInfoCache.Store(base)
			sysInfoCacheAt.Store(time.Now().UnixNano())
		}
	}
	// Refresh runtime sub-struct on every call (cheap).
	cloned := *base
	cloned.Runtime = s.gatherRuntimeInfo()
	return &cloned
}

func (s *Server) gatherRuntimeInfo() *SystemHostRuntimeInfo {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r := &SystemHostRuntimeInfo{
		ProcessUptimeSeconds: int64(time.Since(processStartTime).Seconds()),
		Goroutines:           runtime.NumGoroutine(),
		HeapAllocBytes:       ms.HeapAlloc,
		HeapSysBytes:         ms.HeapSys,
		GCRuns:               ms.NumGC,
	}
	if s != nil && s.db != nil {
		stats := s.db.Stats()
		r.DBPool = &SystemHostDBPoolInfo{
			Open:      stats.OpenConnections,
			InUse:     stats.InUse,
			Idle:      stats.Idle,
			WaitCount: stats.WaitCount,
		}
	}
	return r
}

func gatherSystemInfo() *SystemHostInfo {
	si := &SystemHostInfo{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		SampledAt: time.Now(),
	}
	si.Hostname, _ = os.Hostname()
	if out, err := exec.Command("uname", "-a").Output(); err == nil {
		si.Uname = strings.TrimSpace(string(out))
	}
	si.Version = SystemHostVersionInfo{
		GitSHA:    buildGitSHA,
		GitBranch: buildGitBranch,
		GitDirty:  strings.EqualFold(buildGitDirty, "true"),
		BuildTime: buildBuildTime,
		Go:        runtime.Version(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		si.Version.Module = info.Main.Path
	}
	si.UptimeSeconds = gatherOSUptime()
	si.IPs = gatherIPs()
	si.Disks = gatherDisks()
	si.Memory = gatherMemory()
	si.CPU = gatherCPU()
	return si
}

func gatherIPs() []SystemHostIPInfo {
	out := make([]SystemHostIPInfo, 0)
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			family := "ipv4"
			if ip.To4() == nil {
				family = "ipv6"
			}
			out = append(out, SystemHostIPInfo{
				Interface: iface.Name,
				Address:   ip.String(),
				Family:    family,
			})
		}
	}
	return out
}

// gatherDisks shells out to df -kP so the output is consistent across
// darwin and linux. Filters out devfs / autofs / overlay / map-* and
// macOS read-only system-snapshot mounts (they always show 0 free and
// just add noise).
func gatherDisks() []SystemHostDiskInfo {
	out := make([]SystemHostDiskInfo, 0)
	cmd := exec.Command("df", "-kP")
	raw, err := cmd.Output()
	if err != nil {
		return out
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return out
	}
	skipFS := map[string]bool{"devfs": true, "autofs": true, "overlay": true, "tmpfs": true}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		fs := fields[0]
		if skipFS[fs] || strings.HasPrefix(fs, "map ") || strings.HasPrefix(fs, "map auto_") {
			continue
		}
		blocks, _ := strconv.ParseUint(fields[1], 10, 64)
		used, _ := strconv.ParseUint(fields[2], 10, 64)
		avail, _ := strconv.ParseUint(fields[3], 10, 64)
		mount := strings.Join(fields[5:], " ")
		if blocks == 0 {
			continue
		}
		// Hide macOS system pseudo-volumes (Preboot/VM/xarts/Hardware/etc).
		// /System/Volumes/Data is the real writable APFS volume on modern
		// macOS — keep it. /FactoryLogs is empty noise — hide.
		if strings.HasPrefix(mount, "/System/Volumes/") && mount != "/System/Volumes/Data" {
			continue
		}
		if mount == "/FactoryLogs" {
			continue
		}
		total := blocks * 1024
		usedB := used * 1024
		freeB := avail * 1024
		out = append(out, SystemHostDiskInfo{
			Mount:      mount,
			Filesystem: fs,
			TotalBytes: total,
			UsedBytes:  usedB,
			FreeBytes:  freeB,
			UsedPct:    pct(usedB, total),
		})
	}
	return out
}

func gatherMemory() SystemHostMemoryInfo {
	var m SystemHostMemoryInfo
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			m.TotalBytes, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		}
		// vm_stat output → derive available = (free + inactive + speculative) * pagesize
		if out, err := exec.Command("vm_stat").Output(); err == nil {
			m.AvailableBytes = parseDarwinVMStatAvailable(string(out))
		}
	case "linux":
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			m.TotalBytes, m.AvailableBytes = parseLinuxMeminfo(string(data))
		}
	}
	return m
}

func parseDarwinVMStatAvailable(out string) uint64 {
	pageSize := uint64(16384) // darwin arm64 default; falls back below
	var freePages, inactivePages, speculativePages uint64
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "Mach Virtual Memory Statistics:"):
			// header: "page size of N bytes" appears in some versions
			if idx := strings.Index(l, "page size of "); idx >= 0 {
				rest := l[idx+len("page size of "):]
				rest = strings.TrimSuffix(rest, " bytes)")
				if v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64); err == nil {
					pageSize = v
				}
			}
		case strings.HasPrefix(l, "Pages free:"):
			freePages = vmStatNumber(l)
		case strings.HasPrefix(l, "Pages inactive:"):
			inactivePages = vmStatNumber(l)
		case strings.HasPrefix(l, "Pages speculative:"):
			speculativePages = vmStatNumber(l)
		}
	}
	return (freePages + inactivePages + speculativePages) * pageSize
}

func vmStatNumber(line string) uint64 {
	// "Pages free: 1234." → 1234
	parts := strings.Split(line, ":")
	if len(parts) != 2 {
		return 0
	}
	v := strings.TrimSpace(parts[1])
	v = strings.TrimSuffix(v, ".")
	v = strings.ReplaceAll(v, ",", "")
	n, _ := strconv.ParseUint(v, 10, 64)
	return n
}

func parseLinuxMeminfo(out string) (total, available uint64) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				total = v * 1024
			}
		case "MemAvailable:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				available = v * 1024
			}
		}
	}
	return
}

func gatherCPU() SystemHostCPUInfo {
	c := SystemHostCPUInfo{Count: runtime.NumCPU()}
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			c.Model = strings.TrimSpace(string(out))
		}
	case "linux":
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "model name") {
					if idx := strings.Index(line, ":"); idx >= 0 {
						c.Model = strings.TrimSpace(line[idx+1:])
						break
					}
				}
			}
		}
	}
	return c
}

// gatherOSUptime returns seconds since boot. Uses Sysinfo on linux and
// sysctl kern.boottime on darwin. Returns 0 on platforms we don't
// recognize.
func gatherOSUptime() int64 {
	switch runtime.GOOS {
	case "linux":
		// /proc/uptime → "12345.67 8901.23"; first field = seconds.
		if data, err := os.ReadFile("/proc/uptime"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
					return int64(v)
				}
			}
		}
	case "darwin":
		// sysctl -n kern.boottime → "{ sec = 1747000000, usec = 0 } ..."
		out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
		if err != nil {
			return 0
		}
		s := string(out)
		if idx := strings.Index(s, "sec ="); idx >= 0 {
			rest := s[idx+len("sec ="):]
			rest = strings.TrimSpace(rest)
			if end := strings.IndexAny(rest, ", "); end > 0 {
				rest = rest[:end]
			}
			if boot, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil && boot > 0 {
				return time.Now().Unix() - boot
			}
		}
	}
	return 0
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100.0
}
