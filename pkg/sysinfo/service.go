package sysinfo

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"lightpanel/pkg/helper"
)

var unitPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,240}\.service$`)

func validUnit(s string) bool { return unitPattern.MatchString(s) }

type Manager struct{ Run Runner }

func NewManager() *Manager { return &Manager{Run: RunCommand} }

type Service struct {
	Name          string `json:"name"`
	LoadState     string `json:"load_state"`
	State         string `json:"state"`
	SubState      string `json:"sub_state"`
	Description   string `json:"description"`
	UnitFileState string `json:"unit_file_state"`
}

// mergeServices includes installed units which systemd has not loaded yet.
func mergeServices(loaded, files string) []Service {
	units := make(map[string]Service)
	for _, line := range strings.Split(files, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		units[fields[0]] = Service{Name: fields[0], LoadState: "not-loaded", State: "inactive", SubState: "dead", UnitFileState: fields[1]}
	}
	for _, line := range strings.Split(loaded, "\n") {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "●"))
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		s := units[fields[0]]
		s.Name, s.LoadState, s.State, s.SubState = fields[0], fields[1], fields[2], fields[3]
		s.Description = strings.Join(fields[4:], " ")
		if s.UnitFileState == "" {
			s.UnitFileState = "unknown"
		}
		units[s.Name] = s
	}
	items := make([]Service, 0, len(units))
	for _, unit := range units {
		items = append(items, unit)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

func (m *Manager) Services(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	args := []string{"list-units", "--all", "--type=service", "--no-pager", "--plain", "--no-legend"}
	if name != "" {
		if !validUnit(name) {
			http.Error(w, "invalid .service unit", 400)
			return
		}
		args = []string{"show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState", "--", name}
	}
	out, err := m.Run(r.Context(), "systemctl", args...)
	if err != nil {
		commandError(w, out, err)
		return
	}
	if name != "" {
		JSON(w, map[string]string{"output": out})
		return
	}
	files, err := m.Run(r.Context(), "systemctl", "list-unit-files", "--type=service", "--no-pager", "--plain", "--no-legend")
	if err != nil {
		commandError(w, files, err)
		return
	}
	JSON(w, struct {
		Output string    `json:"output"`
		Items  []Service `json:"items"`
	}{out, mergeServices(out, files)})
}
func (m *Manager) ServiceAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.FormValue("name"), r.FormValue("action")
	if !validUnit(name) || !helper.ValidServiceAction(action) {
		http.Error(w, "invalid unit/action", 400)
		return
	}
	// Unprivileged panels cannot signal systemd directly; the helper applies
	// its per-unit ACL and runs the same command as root.
	if out, routed, err := privileged(r.Context(), helper.Request{Op: helper.OpService, Unit: name, Action: action}); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "service operation completed"})
		return
	}
	out, err := m.Run(r.Context(), "systemctl", "--no-ask-password", action, "--", name)
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"message": "service operation completed"})
}
