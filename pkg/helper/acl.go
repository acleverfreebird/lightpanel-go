package helper

import (
	"fmt"
	"regexp"
	"strings"
)

var unitPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,240}\.service$`)
var userNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUnit matches the systemd unit names the helper will ever act on.
func ValidUnit(s string) bool { return unitPattern.MatchString(s) }

// ValidUserName bounds config-side user names before they are resolved to UIDs.
func ValidUserName(s string) bool { return userNamePattern.MatchString(s) }

// CheckServiceACL enforces the per-service/per-action grant. A unit is
// allowed only when listed exactly (or via the "*" wildcard unit) AND its
// action is in that unit's action list; the wildcard entry grants its actions
// to every unit, so per-unit lists are additive. An empty or absent ACL
// denies everything: grants are opt-in per unit.
func CheckServiceACL(acl map[string][]string, unit, action string) bool {
	if !ValidUnit(unit) || !ValidServiceAction(action) {
		return false
	}
	return actionAllowed(acl[unit], action) || actionAllowed(acl["*"], action)
}

func actionAllowed(actions []string, action string) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}

// ValidateServicesACL checks a configured ACL so a typo fails at config load
// instead of silently denying at runtime.
func ValidateServicesACL(acl map[string][]string) error {
	for unit, actions := range acl {
		if unit != "*" && !ValidUnit(unit) {
			return fmt.Errorf("helper.services: invalid unit %q", unit)
		}
		if len(actions) == 0 {
			continue // an explicit empty grant denies; keep it legal
		}
		for _, a := range actions {
			if !ValidServiceAction(a) {
				return fmt.Errorf("helper.services: unit %q has invalid action %q (allowed: start, stop, restart)", unit, a)
			}
		}
	}
	return nil
}

// FirewallArgs builds the complete argument vector for a firewall command.
// The helper never accepts raw arguments from the panel: it recomputes them
// from the whitelisted (engine, action, port, protocol) tuple.
func FirewallArgs(engine, port, protocol, action string) ([]string, error) {
	if !ValidPortString(port) || (protocol != "tcp" && protocol != "udp") {
		return nil, fmt.Errorf("port must be 1..65535; protocol tcp or udp")
	}
	target := port + "/" + protocol
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

// FirewallStatusArgs is the read-only status query per engine.
func FirewallStatusArgs(engine string) ([]string, error) {
	switch engine {
	case "ufw":
		return []string{"status", "numbered"}, nil
	case "firewalld":
		return []string{"--list-all"}, nil
	}
	return nil, fmt.Errorf("unsupported engine")
}

func firewallBinary(engine string) string {
	if engine == "firewalld" {
		return "firewall-cmd"
	}
	return engine
}

// ValidEngine reports whether engine names a supported firewall frontend.
func ValidEngine(engine string) bool { return engine == "ufw" || engine == "firewalld" }

// ValidAbsPath accepts an absolute, cleaned unix path (no backslash, NUL,
// or "." / ".." components). It is used for the helper socket and staging
// directory and must behave identically on every build platform, so unix
// path semantics are implemented manually rather than via the path/filepath
// of the host OS.
func ValidAbsPath(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	if p != strings.TrimSuffix(p, "/") {
		return false
	}
	if len(p) > 4096 {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// TrimOutput caps embedded command output in errors.
func TrimOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4096 {
		s = s[:4096] + "...(truncated)"
	}
	return s
}
