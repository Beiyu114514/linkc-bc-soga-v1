// Package status samples host CPU/memory/swap/disk/uptime for panel reports.
package status

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Snapshot is one status sample.
type Snapshot struct {
	CPUPercent float64
	MemTotal   int64
	MemUsed    int64
	SwapTotal  int64
	SwapUsed   int64
	DiskTotal  int64
	DiskUsed   int64
	Uptime     int64
}

// Collector keeps the latest sample. CPU is computed between Refresh calls.
type Collector struct {
	mu       sync.Mutex
	diskPath string
	prev     cpuSample
	hasPrev  bool
	snap     Snapshot
}

func New(diskPath string) *Collector {
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{diskPath: diskPath}
}

// Refresh samples the host and stores the result.
func (c *Collector) Refresh() Snapshot {
	cur := readCPUSample()
	memTotal, memAvail, swapTotal, swapFree := readMemInfo()
	diskTotal, diskUsed := readDisk(c.diskPath)
	up := readUptime()

	c.mu.Lock()
	pct := 0.0
	if c.hasPrev && cur.total > c.prev.total {
		dt := float64(cur.total - c.prev.total)
		db := float64(cur.busy - c.prev.busy)
		pct = db / dt * 100
	}
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	c.prev = cur
	c.hasPrev = true
	c.snap = Snapshot{
		CPUPercent: pct,
		MemTotal:   memTotal,
		MemUsed:    memTotal - memAvail,
		SwapTotal:  swapTotal,
		SwapUsed:   swapTotal - swapFree,
		DiskTotal:  diskTotal,
		DiskUsed:   diskUsed,
		Uptime:     up,
	}
	s := c.snap
	c.mu.Unlock()
	return s
}

// Snapshot returns the most recent sample.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

type cpuSample struct {
	total uint64
	busy  uint64
}

func readCPUSample() cpuSample {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		vals = append(vals, v)
	}
	var total, idle uint64
	for i, v := range vals {
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return cpuSample{total: total, busy: total - idle}
}

func readMemInfo() (total, available, swapTotal, swapFree int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	parse := func(name string) int64 {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, name+":") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					v, _ := strconv.ParseInt(f[1], 10, 64)
					return v * 1024
				}
			}
		}
		return 0
	}
	return parse("MemTotal"), parse("MemAvailable"), parse("SwapTotal"), parse("SwapFree")
}

func readDisk(path string) (total, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	avail := int64(st.Bavail) * bs
	return total, total - avail
}

func readUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}
