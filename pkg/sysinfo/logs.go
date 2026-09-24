package sysinfo

import (
	"net/http"
	"strconv"
)

func (m *Manager) Logs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if s := r.URL.Query().Get("lines"); s != "" {
		v, e := strconv.Atoi(s)
		if e != nil || v < 1 || v > 1000 {
			http.Error(w, "lines must be 1..1000", 400)
			return
		}
		n = v
	}
	args := []string{"--no-pager", "--quiet", "--output=short-iso", "--lines=" + strconv.Itoa(n)}
	if name := r.URL.Query().Get("name"); name != "" {
		if !validUnit(name) {
			http.Error(w, "invalid .service unit", 400)
			return
		}
		args = append(args, "--unit="+name)
	}
	out, err := m.Run(r.Context(), "journalctl", args...)
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"output": out})
}
