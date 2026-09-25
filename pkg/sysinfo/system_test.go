//go:build linux

package sysinfo

import (
	"testing"
)

func TestCommandValidation(t *testing.T) {
	for _, s := range []string{"--help", "-x.service", "x;id.service", "../x.service", "*.service", "x.socket"} {
		if validUnit(s) {
			t.Errorf("accepted unit %q", s)
		}
	}
	for _, s := range []string{"sshd.service", "worker@one.service", "dbus-org.example.service"} {
		if !validUnit(s) {
			t.Errorf("rejected unit %q", s)
		}
	}
	for _, args := range [][3]string{{"0", "tcp", "allow"}, {"65536", "tcp", "allow"}, {"22;id", "tcp", "allow"}, {"22", "icmp", "allow"}, {"22", "tcp", "reset"}} {
		if _, err := firewallArgs("ufw", args[0], args[1], args[2]); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if _, err := firewallArgs("firewalld", "22", "tcp", "deny"); err == nil {
		t.Fatal("firewalld deny silently mapped to remove")
	}
}
func TestProcStatWithSpaces(t *testing.T) {
	p, err := parseStat("42 (worker (one) two) S 1 0 0 0 0 0 0 0 0 0 10 20 0 0 0 0 0 0 1234 0 256")
	if err != nil || p.Name != "worker (one) two" || p.StartTime != "1234" || p.RSSBytes != 256*uint64(pageSize) {
		t.Fatalf("%+v %v", p, err)
	}
}
