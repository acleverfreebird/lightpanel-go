package sysinfo

import (
	"net/http"
	"regexp"
)

var unitPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,240}\.service$`)

func validUnit(s string) bool { return unitPattern.MatchString(s) }

type Manager struct{ Run Runner }

func NewManager() *Manager { return &Manager{Run: RunCommand} }
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
	JSON(w, map[string]string{"output": out})
}
func (m *Manager) ServiceAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.FormValue("name"), r.FormValue("action")
	if !validUnit(name) || (action != "start" && action != "stop" && action != "restart") {
		http.Error(w, "invalid unit/action", 400)
		return
	}
	out, err := m.Run(r.Context(), "systemctl", "--no-ask-password", action, "--", name)
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"message": "service operation completed"})
}
