//go:build linux

package sysinfo

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Metrics struct {
	Hostname      string    `json:"hostname"`
	OS            string    `json:"os"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryTotal   uint64    `json:"memory_total"`
	MemoryUsed    uint64    `json:"memory_used"`
	DiskTotal     uint64    `json:"disk_total"`
	DiskUsed      uint64    `json:"disk_used"`
	Load          []float64 `json:"load"`
	Uptime        float64   `json:"uptime"`
	RxBytesPerSec float64   `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64   `json:"tx_bytes_per_sec"`
	SelfRSS       uint64    `json:"self_rss"`
	SampleReady   bool      `json:"sample_ready"`
	Errors        []string  `json:"errors"`
}

// On-demand shared cache: no permanent sampler goroutine, at most one sample/2s.
type MetricsReader struct {
	mu                  sync.Mutex
	at                  time.Time
	cpuAt, netAt        time.Time
	total, idle, rx, tx uint64
	cached              Metrics
}

func number(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
func cpuCounters(s string) (uint64, uint64, error) {
	line, _, _ := strings.Cut(s, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid /proc/stat")
	}
	var total, idle uint64
	for i := 1; i < len(f) && i <= 8; i++ {
		v, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += v
		if i == 4 || i == 5 {
			idle += v
		}
	}
	return total, idle, nil // guest/guest_nice are already included in user/nice.
}
func netCounters(s string) (uint64, uint64) {
	var rx, tx uint64
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 16 {
			rx += number(f[0])
			tx += number(f[8])
		}
	}
	return rx, tx
}
func (m *MetricsReader) Read() Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if !m.at.IsZero() && now.Sub(m.at) < 2*time.Second {
		return m.cached
	}
	v := Metrics{Load: []float64{0, 0, 0}, Errors: []string{}}
	v.Hostname, _ = os.Hostname()
	read := func(path string) string {
		b, err := readLimited(path, 1<<20)
		if err != nil {
			v.Errors = append(v.Errors, path+": "+err.Error())
		}
		return string(b)
	}
	for _, line := range strings.Split(read("/etc/os-release"), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			v.OS = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
			break
		}
	}
	total, idle, err := cpuCounters(read("/proc/stat"))
	if err == nil {
		if !m.cpuAt.IsZero() && total > m.total && idle >= m.idle && idle-m.idle <= total-m.total {
			v.CPUPercent = 100 * float64(total-m.total-(idle-m.idle)) / float64(total-m.total)
			v.SampleReady = true
		}
		m.total, m.idle, m.cpuAt = total, idle, now
	}
	mem := map[string]uint64{}
	for _, line := range strings.Split(read("/proc/meminfo"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			mem[strings.TrimSuffix(f[0], ":")] = number(f[1]) * 1024
		}
	}
	v.MemoryTotal = mem["MemTotal"]
	available, ok := mem["MemAvailable"]
	if !ok {
		available = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	v.MemoryUsed = v.MemoryTotal - min(available, v.MemoryTotal)
	f := strings.Fields(read("/proc/loadavg"))
	for i := 0; i < 3 && i < len(f); i++ {
		v.Load[i], _ = strconv.ParseFloat(f[i], 64)
	}
	f = strings.Fields(read("/proc/uptime"))
	if len(f) > 0 {
		v.Uptime, _ = strconv.ParseFloat(f[0], 64)
	}
	netData := read("/proc/net/dev")
	if netData != "" {
		rx, tx := netCounters(netData)
		dt := now.Sub(m.netAt).Seconds()
		if !m.netAt.IsZero() && dt > 0 && rx >= m.rx && tx >= m.tx {
			v.RxBytesPerSec = float64(rx-m.rx) / dt
			v.TxBytesPerSec = float64(tx-m.tx) / dt
		}
		m.rx, m.tx, m.netAt = rx, tx, now
	}
	var stat unix.Statfs_t
	if err := unix.Statfs("/", &stat); err == nil {
		v.DiskTotal = stat.Blocks * uint64(stat.Bsize)
		v.DiskUsed = (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
	} else {
		v.Errors = append(v.Errors, err.Error())
	}
	f = strings.Fields(read("/proc/self/statm"))
	if len(f) > 1 {
		v.SelfRSS = number(f[1]) * uint64(os.Getpagesize())
	}
	m.at, m.cached = now, v
	return v
}
func (m *MetricsReader) ServeHTTP(w http.ResponseWriter, r *http.Request) { JSON(w, m.Read()) }
