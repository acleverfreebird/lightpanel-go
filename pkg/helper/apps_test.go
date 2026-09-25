package helper

import (
	"reflect"
	"testing"
)

func TestValidAppName(t *testing.T) {
	for _, name := range []string{"nginx", "apache", "docker", "certbot"} {
		if !ValidAppName(name) {
			t.Errorf("ValidAppName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "nginx ", "Nginx", "nginx-extra", "nginx;rm", "certbot\n", "../../etc"} {
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
		want         [][]string
	}{
		{"apt-get", "nginx", [][]string{{"apt-get", "update"}, {"apt-get", "install", "-y", "nginx"}}},
		{"apt-get", "apache", [][]string{{"apt-get", "update"}, {"apt-get", "install", "-y", "apache2"}}},
		{"apt-get", "docker", [][]string{{"apt-get", "update"}, {"apt-get", "install", "-y", "docker.io"}}},
		{"apt-get", "certbot", [][]string{{"apt-get", "update"}, {"apt-get", "install", "-y", "certbot"}}},
		{"dnf", "apache", [][]string{{"dnf", "install", "-y", "httpd"}}},
		{"yum", "apache", [][]string{{"yum", "install", "-y", "httpd"}}},
		{"dnf", "nginx", [][]string{{"dnf", "install", "-y", "nginx"}}},
		{"zypper", "docker", [][]string{{"zypper", "--non-interactive", "install", "docker"}}},
		{"apk", "certbot", [][]string{{"apk", "add", "certbot"}}},
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
		{"apt-get", ""}, {"apt-get", "mysql"}, {"apt-get", "nginx; rm -rf /"},
	} {
		if _, err := AppInstallSteps(c.manager, c.app); err == nil {
			t.Errorf("AppInstallSteps(%q, %q) accepted, want rejection", c.manager, c.app)
		}
	}
}
