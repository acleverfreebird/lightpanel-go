package sysinfo

import (
	"fmt"
	"net/http"
	"strconv"
)

func firewallArgs(engine, port, protocol, action string) ([]string, error) {
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || (protocol != "tcp" && protocol != "udp") {
		return nil, fmt.Errorf("port must be 1..65535; protocol tcp or udp")
	}
	target := strconv.Itoa(p) + "/" + protocol
	switch engine {
	case "ufw":
		switch action {
		case "allow", "deny":
			return []string{action, target}, nil
		case "remove-allow":
			return []string{"--force", "delete", "allow", target}, nil
		case "remove-deny":
			return []string{"--force", "delete", "deny", target}, nil
		}
	case "firewalld":
		switch action {
		case "allow":
			return []string{"--add-port=" + target}, nil
		case "remove-allow":
			return []string{"--remove-port=" + target}, nil
		}
	}
	return nil, fmt.Errorf("unsupported engine/action (firewalld supports allow/remove-allow only)")
}
func detectEngine() string {
	if _, e := executable("firewall-cmd"); e == nil {
		return "firewalld"
	}
	if _, e := executable("ufw"); e == nil {
		return "ufw"
	}
	return "none"
}
func engineRequest(r *http.Request) string {
	e := r.FormValue("engine")
	if e == "" {
		return detectEngine()
	}
	return e
}
func (m *Manager) Firewall(w http.ResponseWriter, r *http.Request) {
	engine := engineRequest(r)
	var out string
	var err error
	switch engine {
	case "ufw":
		out, err = m.Run(r.Context(), "ufw", "status", "numbered")
	case "firewalld":
		out, err = m.Run(r.Context(), "firewall-cmd", "--list-all")
	default:
		http.Error(w, "no supported firewall detected", 501)
		return
	}
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"engine": engine, "output": out})
}
func (m *Manager) FirewallAction(w http.ResponseWriter, r *http.Request) {
	engine := engineRequest(r)
	args, err := firewallArgs(engine, r.FormValue("port"), r.FormValue("protocol"), r.FormValue("action"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cmd := "ufw"
	if engine == "firewalld" {
		cmd = "firewall-cmd"
	}
	out, err := m.Run(r.Context(), cmd, args...)
	if err != nil {
		commandError(w, out, err)
		return
	}
	message := "rule updated"
	if engine == "firewalld" {
		message = "runtime rule updated in default zone; reload/reboot discards it"
	}
	JSON(w, map[string]string{"message": message})
}
