package helper

import (
	"fmt"
)

// The app store: a fixed catalog of server software installable through the
// system package manager. Both the panel and the helper compile this file, so
// they agree on the app keys and on the exact argv built for an install — the
// panel only ever sends the app name and the helper recomputes every argument.

// AppSpec is one installable application. Packages maps a package manager
// binary (see PackageManagerBinaries) to the distro-specific package name.
type AppSpec struct {
	Name        string
	Title       string
	Description string
	Packages    map[string]string
}

// AppCatalog is the installable app list, in display order. Only keys
// accepted by ValidAppName may appear here.
var AppCatalog = []AppSpec{
	{Name: "nginx", Title: "Nginx", Description: "高性能网页服务器与反向代理，站点管理首选引擎", Packages: map[string]string{
		"apt-get": "nginx", "dnf": "nginx", "yum": "nginx", "zypper": "nginx", "apk": "nginx",
	}},
	{Name: "apache", Title: "Apache", Description: "Apache HTTP 服务器（Debian 系为 apache2，RHEL 系为 httpd）", Packages: map[string]string{
		"apt-get": "apache2", "dnf": "httpd", "yum": "httpd", "zypper": "apache2", "apk": "apache2",
	}},
	{Name: "docker", Title: "Docker", Description: "容器运行时，用于以容器方式部署站点与应用", Packages: map[string]string{
		"apt-get": "docker.io", "dnf": "docker", "yum": "docker", "zypper": "docker", "apk": "docker",
	}},
	{Name: "certbot", Title: "Certbot", Description: "Let's Encrypt 客户端，为站点签发免费的 HTTPS 证书", Packages: map[string]string{
		"apt-get": "certbot", "dnf": "certbot", "yum": "certbot", "zypper": "certbot", "apk": "certbot",
	}},
}

// PackageManagerBinaries lists the supported package managers in detection
// order. Each entry is also the exact binary name executed.
var PackageManagerBinaries = []string{"apt-get", "dnf", "yum", "zypper", "apk"}

// AppSpecByName returns the catalog entry for a whitelisted app name.
func AppSpecByName(name string) (AppSpec, bool) {
	for _, spec := range AppCatalog {
		if spec.Name == name {
			return spec, true
		}
	}
	return AppSpec{}, false
}

// ValidAppName reports whether name is exactly one catalog app key. App names
// become parts of argv, so the alphabet stays closed.
func ValidAppName(name string) bool {
	_, ok := AppSpecByName(name)
	return ok
}

// ValidPackageManager reports whether manager is one of the supported
// package manager binaries.
func ValidPackageManager(manager string) bool {
	for _, bin := range PackageManagerBinaries {
		if bin == manager {
			return true
		}
	}
	return false
}

// AppInstallSteps builds the complete, whitelisted argv sequence to install
// app with manager. Nothing here accepts panel-supplied strings beyond the
// app key, and the package name always comes from this catalog. apt-get
// refreshes the package lists first (a stale list is the most common reason a
// fresh install fails); its failure is non-fatal, matching a manual install.
func AppInstallSteps(manager, app string) ([][]string, error) {
	if !ValidPackageManager(manager) {
		return nil, fmt.Errorf("unsupported package manager %q", manager)
	}
	spec, ok := AppSpecByName(app)
	if !ok {
		return nil, fmt.Errorf("unknown app %q", app)
	}
	pkg, ok := spec.Packages[manager]
	if !ok || pkg == "" {
		return nil, fmt.Errorf("app %s has no package for %s", app, manager)
	}
	switch manager {
	case "apt-get":
		return [][]string{
			{"apt-get", "update"},
			{"apt-get", "install", "-y", pkg},
		}, nil
	case "dnf", "yum":
		return [][]string{{manager, "install", "-y", pkg}}, nil
	case "zypper":
		return [][]string{{"zypper", "--non-interactive", "install", pkg}}, nil
	case "apk":
		return [][]string{{"apk", "add", pkg}}, nil
	}
	return nil, fmt.Errorf("unsupported package manager %q", manager)
}
