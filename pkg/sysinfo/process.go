//go:build linux

package sysinfo

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var pageSize = os.Getpagesize()

type Process struct {
	PID       int    `json:"pid"`
	Name      string `json:"name"`
	State     string `json:"state"`
	UID       string `json:"uid"`
	RSSBytes  uint64 `json:"rss_bytes"`
	StartTime string `json:"start_time"`
}

func parseStat(s string) (Process, error) {
	var p Process
	left, right := strings.Index(s, "("), strings.LastIndex(s, ")")
	if left < 1 || right <= left {
		return p, fmt.Errorf("invalid stat")
	}
	pid, e := strconv.Atoi(strings.TrimSpace(s[:left]))
	if e != nil {
		return p, e
	}
	f := strings.Fields(s[right+1:])
	if len(f) < 22 {
		return p, fmt.Errorf("short stat")
	}
	rss, e := strconv.ParseUint(f[21], 10, 64)
	if e != nil {
		return p, e
	}
	return Process{PID: pid, Name: s[left+1 : right], State: f[0], RSSBytes: rss * uint64(pageSize), StartTime: f[19]}, nil
}
func ListProcesses(query string) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	list := make([]Process, 0)
	query = strings.ToLower(query)
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		b, err := readLimited("/proc/"+entry.Name()+"/stat", 8192)
		if err != nil {
			continue
		}
		p, err := parseStat(string(b))
		if err != nil {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(p.Name), query) && !strings.Contains(entry.Name(), query) {
			continue
		}
		status, _ := readLimited("/proc/"+entry.Name()+"/status", 16384)
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				f := strings.Fields(line)
				if len(f) > 1 {
					p.UID = f[1]
				}
				break
			}
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].RSSBytes == list[j].RSSBytes {
			return list[i].PID < list[j].PID
		}
		return list[i].RSSBytes > list[j].RSSBytes
	})
	return list, nil
}
func HandleProcesses(w http.ResponseWriter, r *http.Request) {
	list, err := ListProcesses(r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, "cannot read /proc", 500)
		return
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		page, err = strconv.Atoi(v)
		if err != nil || page < 1 || page > 100000 {
			http.Error(w, "invalid page", 400)
			return
		}
	}
	start := (page - 1) * 100
	if start > len(list) {
		start = len(list)
	}
	end := min(start+100, len(list))
	JSON(w, map[string]any{"items": list[start:end], "total": len(list), "page": page})
}
func HandleProcessKill(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(r.FormValue("pid"))
	sig, errSig := strconv.Atoi(r.FormValue("signal"))
	if err != nil || pid <= 1 || pid == os.Getpid() || errSig != nil || (sig != 15 && sig != 9) || r.FormValue("start_time") == "" {
		http.Error(w, "invalid PID/signal/process identity", 400)
		return
	}
	// Pin process identity before checking /proc. Never fall back to racy kill(pid).
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ENOSYS {
			http.Error(w, "safe signaling requires Linux 5.3+", 501)
		} else {
			http.Error(w, "process unavailable or permission denied", 409)
		}
		return
	}
	defer unix.Close(fd)
	b, err := readLimited("/proc/"+strconv.Itoa(pid)+"/stat", 8192)
	if err != nil {
		http.Error(w, "process disappeared", 409)
		return
	}
	p, err := parseStat(string(b))
	if err != nil || p.StartTime != r.FormValue("start_time") {
		http.Error(w, "process changed; refresh first", 409)
		return
	}
	if err = unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0); err != nil {
		http.Error(w, "signal failed: "+err.Error(), 409)
		return
	}
	JSON(w, map[string]string{"message": "signal sent; process may take time to exit"})
}
