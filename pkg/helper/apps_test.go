package helper

import (
	"reflect"
	"testing"
)

func TestValidAppName(t *testing.T) {
	for _, name := range []string{"nginx", "apache", "docker", "certbot", "mysql", "mariadb", "postgresql", "redis"} {
		if !ValidAppName(name) {
			t.Errorf("ValidAppName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "nginx ", "Nginx", "nginx-extra", "nginx;rm", "certbot\n", "../../etc", "mysql8", "postgres", "mongodb"} {
		if ValidAppName(name) {
			t.Errorf("ValidAppName(%q) = true, want false", name)
		}
	}
}

func TestValidPackageManager(t *testing.T) {
	for _, bin := range PackageManagerBinaries {
		if !ValidPackageManager(bin) {
			t.Errorf("ValidPackageManager(%q) = false, want true", bin)
		}
	}
	for _, bin := range []string{"", "apt", "pacman", "apt-get ", "dnf5"} {
		if ValidPackageManager(bin) {
			t.Errorf("ValidPackageManager(%q) = true, want false", bin)
		}
	}
}

func TestAppInstallSteps(t *testing.T) {
	cases := []struct {
		manager, app string
		want         []Step
	}{
		{"apt-get", "nginx", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "nginx"}},
		}},
		{"apt-get", "mysql", []Step{
			{Args: []string{"sh", "-c", mysqlConfigOverrideScript}},
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "mysql-server"}},
			{Args: []string{"sh", "-c", mysqlDatadirInitScript}},
			{Args: []string{"sh", "-c", mysqlUnitPatchScript}},
		}},
		{"apt-get", "apache", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "apache2"}},
		}},
		// Only mysql gets the unprivileged-container fixups.
		{"apt-get", "mariadb", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "mariadb-server"}},
		}},
		{"apt-get", "docker", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "docker.io"}},
		}},
		{"apt-get", "redis", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", "redis-server"}},
		}},
		{"dnf", "postgresql", []Step{{Args: []string{"dnf", "install", "-y", "postgresql-server"}}}},
		{"dnf", "apache", []Step{{Args: []string{"dnf", "install", "-y", "httpd"}}}},
		{"yum", "apache", []Step{{Args: []string{"yum", "install", "-y", "httpd"}}}},
		{"dnf", "nginx", []Step{{Args: []string{"dnf", "install", "-y", "nginx"}}}},
		{"zypper", "docker", []Step{{Args: []string{"zypper", "--non-interactive", "install", "docker"}}}},
		{"apk", "mariadb", []Step{{Args: []string{"apk", "add", "mariadb"}}}},
		{"apk", "certbot", []Step{{Args: []string{"apk", "add", "certbot"}}}},
	}
	for _, c := range cases {
		got, err := AppInstallSteps(c.manager, c.app)
		if err != nil {
			t.Errorf("AppInstallSteps(%q, %q): unexpected error %v", c.manager, c.app, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("AppInstallSteps(%q, %q) = %v, want %v", c.manager, c.app, got, c.want)
		}
	}
	for _, c := range []struct{ manager, app string }{
		{"", "nginx"}, {"apt", "nginx"}, {"pacman", "nginx"},
		{"apt-get", ""}, {"apt-get", "mysql8"}, {"apt-get", "nginx; rm -rf /"},
		{"apk", "mysql"}, {"zypper", "mysql"}, // no package mapped for these managers
	} {
		if _, err := AppInstallSteps(c.manager, c.app); err == nil {
			t.Errorf("AppInstallSteps(%q, %q) accepted, want rejection", c.manager, c.app)
		}
		if _, err := AppRemoveSteps(c.manager, c.app); err == nil {
			t.Errorf("AppRemoveSteps(%q, %q) accepted, want rejection", c.manager, c.app)
		}
	}
}

func TestAppRemoveSteps(t *testing.T) {
	cases := []struct {
		manager, app string
		want         []Step
	}{
		{"apt-get", "nginx", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "purge", "-y", "nginx"}},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "autoremove", "-y"}, Optional: true},
		}},
		{"apt-get", "mysql", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "purge", "-y", "mysql-server"}},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "autoremove", "-y"}, Optional: true},
		}},
		{"apt-get", "apache", []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "purge", "-y", "apache2"}},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "autoremove", "-y"}, Optional: true},
		}},
		{"dnf", "nginx", []Step{
			{Args: []string{"dnf", "remove", "-y", "nginx"}},
			{Args: []string{"dnf", "autoremove", "-y"}, Optional: true},
		}},
		{"yum", "apache", []Step{
			{Args: []string{"yum", "remove", "-y", "httpd"}},
			{Args: []string{"yum", "autoremove", "-y"}, Optional: true},
		}},
		{"zypper", "docker", []Step{{Args: []string{"zypper", "--non-interactive", "remove", "docker"}}}},
		{"apk", "certbot", []Step{{Args: []string{"apk", "del", "certbot"}}}},
	}
	for _, c := range cases {
		got, err := AppRemoveSteps(c.manager, c.app)
		if err != nil {
			t.Errorf("AppRemoveSteps(%q, %q): unexpected error %v", c.manager, c.app, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("AppRemoveSteps(%q, %q) = %v, want %v", c.manager, c.app, got, c.want)
		}
	}
}

func TestAppGroupConflict(t *testing.T) {
	// A web server already installed blocks the other one.
	if got := AppGroupConflict("apache", map[string]bool{"nginx": true}); got != "nginx" {
		t.Errorf("AppGroupConflict(apache, nginx installed) = %q, want %q", got, "nginx")
	}
	if got := AppGroupConflict("nginx", map[string]bool{"apache": true}); got != "apache" {
		t.Errorf("AppGroupConflict(nginx, apache installed) = %q, want %q", got, "apache")
	}
	// Installing an app over its own installed self is a reinstall, not a conflict.
	if got := AppGroupConflict("nginx", map[string]bool{"nginx": true}); got != "" {
		t.Errorf("AppGroupConflict(nginx, nginx installed) = %q, want empty", got)
	}
	// Group-less apps and apps without an installed sibling never conflict.
	if got := AppGroupConflict("mysql", map[string]bool{"nginx": true, "mariadb": true}); got != "" {
		t.Errorf("AppGroupConflict(mysql, others installed) = %q, want empty", got)
	}
	if got := AppGroupConflict("docker", map[string]bool{"nginx": true}); got != "" {
		t.Errorf("AppGroupConflict(docker, nginx installed) = %q, want empty", got)
	}
	if got := AppGroupConflict("nginx", nil); got != "" {
		t.Errorf("AppGroupConflict(nginx, nothing installed) = %q, want empty", got)
	}
	// Unknown app names stay closed.
	if got := AppGroupConflict("haproxy", nil); got != "" {
		t.Errorf("AppGroupConflict(haproxy) = %q, want empty", got)
	}
}
