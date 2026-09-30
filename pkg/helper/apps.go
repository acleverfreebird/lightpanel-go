package helper

import (
	"fmt"
)

// The app store: a fixed catalog of server software installable through the
// system package manager. Both the panel and the helper compile this file, so
// they agree on the app keys and on the exact argv built for an install or a
// removal — the panel only ever sends the app name and the helper recomputes
// every argument.

// AppSpec is one installable application. Packages maps a package manager
// binary (see PackageManagerBinaries) to the distro-specific package name.
// Group names an exclusion class: at most one app per group can be installed
// at a time (the two web servers fight over ports 80/443 and the site
// configuration in sites.go targets exactly one engine).
type AppSpec struct {
	Name        string
	Title       string
	Description string
	Group       string
	Packages    map[string]string
}

// AppCatalog is the installable app list, in display order. Only keys
// accepted by ValidAppName may appear here.
var AppCatalog = []AppSpec{
	{Name: "nginx", Title: "Nginx", Group: "web", Description: "高性能网页服务器与反向代理，站点管理首选引擎", Packages: map[string]string{
		"apt-get": "nginx", "dnf": "nginx", "yum": "nginx", "zypper": "nginx", "apk": "nginx",
	}},
	{Name: "apache", Title: "Apache", Group: "web", Description: "Apache HTTP 服务器（Debian 系为 apache2，RHEL 系为 httpd）", Packages: map[string]string{
		"apt-get": "apache2", "dnf": "httpd", "yum": "httpd", "zypper": "apache2", "apk": "apache2",
	}},
	{Name: "docker", Title: "Docker", Description: "容器运行时，用于以容器方式部署站点与应用", Packages: map[string]string{
		"apt-get": "docker.io", "dnf": "docker", "yum": "docker", "zypper": "docker", "apk": "docker",
	}},
	{Name: "mysql", Title: "MySQL", Description: "广泛使用的关系型数据库，安装后可在「数据库管理」中建库与管用户", Packages: map[string]string{
		"apt-get": "mysql-server", "dnf": "mysql-server", "yum": "mysql-server",
	}},
	{Name: "mariadb", Title: "MariaDB", Description: "MySQL 兼容分支，各发行版官方仓库均有提供", Packages: map[string]string{
		"apt-get": "mariadb-server", "dnf": "mariadb-server", "yum": "mariadb-server", "zypper": "mariadb", "apk": "mariadb",
	}},
	{Name: "postgresql", Title: "PostgreSQL", Description: "功能最先进的开源关系型数据库，安装后可在「数据库管理」中管理", Packages: map[string]string{
		"apt-get": "postgresql", "dnf": "postgresql-server", "yum": "postgresql-server", "zypper": "postgresql-server", "apk": "postgresql",
	}},
	{Name: "redis", Title: "Redis", Description: "内存键值数据库，常用作缓存与消息队列", Packages: map[string]string{
		"apt-get": "redis-server", "dnf": "redis", "yum": "redis", "zypper": "redis", "apk": "redis",
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

// AppGroupConflict returns the name of an already installed app that blocks
// installing app (same catalog group), or "" when app may be installed.
func AppGroupConflict(app string, installed map[string]bool) string {
	spec, ok := AppSpecByName(app)
	if !ok || spec.Group == "" {
		return ""
	}
	for _, other := range AppCatalog {
		if other.Name != app && other.Group == spec.Group && installed[other.Name] {
			return other.Name
		}
	}
	return ""
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

// aptSandboxOpt disables apt's download sandbox (drop to _apt, uid 42),
// which fails with "seteuid 42 failed" in containers lacking CAP_SETUID.
const aptSandboxOpt = "APT::Sandbox::User=root"

// The mysql fixups below exist for the same class of hosts as aptSandboxOpt:
// containers that cannot drop privileges (no CAP_SETUID/CAP_SETGID). The
// stock mysql-server package cannot configure there, in two separate places:
// its mysqld.cnf sets user = mysql (so the postinst's startup test
// "mysqld --verbose --help" calls setuid(2) and aborts), and its postinst
// passes --user=mysql on the command line (start_server, run_init_sql, the
// datadir init) — an argument no config file can override. Either failure
// leaves the package half-configured, and because apt retries the failed
// postinst inside every later transaction, one broken install wedges all
// following installs AND uninstalls on the host.
//
// mysqld itself handles running as root cleanly: check_user() in
// sql/mysqld.cc returns early for the user option "root" and skips every
// privilege change, and it merely warns when not started as root. Two
// cooperating pieces point the package scripts at that:
//
//  1. mysqlCompatSetupScript writes user = root into
//     /etc/mysql/mysql.conf.d/zz-lightpanel.cnf (sorted after the packaged
//     mysqld.cnf, so it wins) — that fixes config-only invocations — and
//     installs a dpkg-diverted /usr/sbin/mysqld wrapper that appends
//     --user=root for root callers, overriding the postinst's command-line
//     --user=mysql (mysqld resolves repeated scalar options last-wins).
//     dpkg-divert keeps package upgrades from overwriting the wrapper.
//  2. mysqlUnitPatchScript switches the systemd unit from User/Group=mysql
//     to root so systemd's own setuid (equally unavailable there) does not
//     stop the panel from starting MySQL.
//
// mysqlDatadirInitScript then initializes the system tables when the
// package's own init could not (its "--initialize-insecure --user=mysql" is
// swallowed by "|| true", leaving an empty datadir behind) and hands
// root-created files back to the mysql user.
//
// The setup and unit scripts begin by probing whether privilege dropping
// works at all (su to nobody). On ordinary hosts the probe succeeds and
// nothing is patched; only hosts where the stock layout cannot work are
// modified.

// mysqlPrivDropProbe succeeds only where setuid(2) is possible at all; on
// such hosts none of the mysql fixups may apply.
const mysqlPrivDropProbe = `su -s /bin/sh nobody -c true >/dev/null 2>&1 && exit 0; `

// mysqlWrapper becomes /usr/sbin/mysqld while the real binary is diverted to
// mysqld.distrib. It only adjusts --user for root callers on hosts where the
// privilege drop itself is broken; everything else passes through. mysqld
// resolves repeated scalar options last-wins, so appending --user=root
// overrides any earlier --user=mysql without touching the argument list.
const mysqlWrapper = `#!/bin/sh
# lightpanel: installed only on hosts that cannot drop privileges, where
# mysqld's --user=<name> dies with "setuid: Operation not permitted". mysqld
# skips every privilege change when the user option is "root" (check_user in
# sql/mysqld.cc).
PATH=/usr/sbin:/usr/bin:/sbin:/bin
if [ "$(id -u)" = 0 ] && ! su -s /bin/sh nobody -c true >/dev/null 2>&1; then
	exec /usr/sbin/mysqld.distrib "$@" --user=root
fi
exec /usr/sbin/mysqld.distrib "$@"
`

// mysqlCompatSetupScript installs the mysqld wrapper and the config override.
// It runs before any dpkg step in BOTH the install and the remove sequence:
// remove needs it just as much, because apt retries the half-configured
// package's postinst inside the purge itself.
const mysqlCompatSetupScript = mysqlPrivDropProbe + `
dpkg-divert --local --rename --add --divert /usr/sbin/mysqld.distrib /usr/sbin/mysqld >/dev/null 2>&1 || true
cat > /usr/sbin/mysqld <<'LIGHTPANEL_WRAPPER'
` + mysqlWrapper + `LIGHTPANEL_WRAPPER
chmod 0755 /usr/sbin/mysqld
mkdir -p /etc/mysql/mysql.conf.d
printf '[mysqld]\nuser = root\n' > /etc/mysql/mysql.conf.d/zz-lightpanel.cnf
[ ! -x /usr/sbin/mysqld.distrib ] || /usr/sbin/mysqld --version >/dev/null 2>&1 || exit 1
`

// mysqlDatadirInitScript initializes the system tables when the package's
// own init could not (its "mysqld --initialize-insecure --user=mysql" is
// swallowed by "|| true" on these hosts, leaving an empty datadir behind).
// Root-created files are handed back to mysql so they match the layout the
// package expects.
const mysqlDatadirInitScript = `if [ ! -d /var/lib/mysql/mysql ]; then
	/usr/sbin/mysqld --initialize-insecure --user=root &&
	chown -R mysql:mysql /var/lib/mysql
fi
`

// mysqlUnitPatchScript switches the service unit from User/Group=mysql to
// root so systemd's own setuid (equally unavailable there) does not stop the
// panel from starting MySQL.
const mysqlUnitPatchScript = mysqlPrivDropProbe + `
for f in /lib/systemd/system/mysql.service /usr/lib/systemd/system/mysql.service; do
	if [ -f "$f" ]; then
		sed -i -e 's/^User=mysql.*/User=root/' -e 's/^Group=mysql.*/Group=root/' "$f"
	fi
done
systemctl daemon-reload >/dev/null 2>&1 || true
`

// Step is one whitelisted command in an install or removal sequence.
// Optional steps are housekeeping whose failure does not abort the job —
// package-manager output is still captured — while a required step's failure
// fails the whole operation.
type Step struct {
	Args     []string
	Optional bool
}

// AppInstallSteps builds the complete, whitelisted step sequence to install
// app with manager, following the package managers' own recovery guidance:
// apt runs `dpkg --configure -a` first so an interrupted earlier install (the
// usual way MySQL leaves a half-configured dpkg behind) cannot wedge the new
// one, then refreshes the package lists (a stale list is the most common
// reason a fresh install fails). MySQL additionally gets the unprivileged-
// container fixups defined above the install/remove builders. Nothing here
// accepts panel-supplied strings beyond the app key, and the package name
// always comes from this catalog.
func AppInstallSteps(manager, app string) ([]Step, error) {
	pkg, err := lookupAppPackage(manager, app)
	if err != nil {
		return nil, err
	}
	switch manager {
	case "apt-get":
		// APT::Sandbox::User=root stops apt from dropping privileges to the
		// _apt user (uid 42) for downloads; in unprivileged containers
		// (no CAP_SETUID) that seteuid fails and every fetch method dies.
		steps := []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "update"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "install", "-y", pkg}},
		}
		if app == "mysql" {
			// The compat setup goes first so the leading dpkg repair and
			// the install's postinst both see a mysqld that can start —
			// this is also what un-wedges a dpkg left half-configured by an
			// earlier, pre-fix install attempt. The datadir init and unit
			// patch repair whatever the package's own scripts had to skip.
			// See the mysql fixup scripts above for why.
			steps = append([]Step{
				{Args: []string{"sh", "-c", mysqlCompatSetupScript}},
			}, steps...)
			steps = append(steps,
				Step{Args: []string{"sh", "-c", mysqlDatadirInitScript}},
				Step{Args: []string{"sh", "-c", mysqlUnitPatchScript}},
			)
		}
		return steps, nil
	case "dnf", "yum":
		return []Step{{Args: []string{manager, "install", "-y", pkg}}}, nil
	case "zypper":
		return []Step{{Args: []string{"zypper", "--non-interactive", "install", pkg}}}, nil
	case "apk":
		return []Step{{Args: []string{"apk", "add", pkg}}}, nil
	}
	return nil, fmt.Errorf("unsupported package manager %q", manager)
}

// AppRemoveSteps builds the complete, whitelisted step sequence to uninstall
// app with manager, mirroring AppInstallSteps. apt-get uses purge so a failed
// install's leftover conffiles and package state are removed too — the
// primary use case is recovering from an install that half-configured — and
// then clears now-orphaned dependencies with autoremove, per Debian's
// official guidance. MySQL removal gets the same compat setup FIRST as the
// install: a half-configured mysql-server-8.0 is retried by dpkg inside the
// purge itself (and inside the leading dpkg repair), so on hosts without
// privilege dropping the removal can only succeed once the mysqld wrapper
// and config override are in place. dnf/yum follow the same remove +
// autoremove pattern.
func AppRemoveSteps(manager, app string) ([]Step, error) {
	pkg, err := lookupAppPackage(manager, app)
	if err != nil {
		return nil, err
	}
	switch manager {
	case "apt-get":
		steps := []Step{
			{Args: []string{"dpkg", "--configure", "-a"}, Optional: true},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "purge", "-y", pkg}},
			{Args: []string{"apt-get", "-o", aptSandboxOpt, "autoremove", "-y"}, Optional: true},
		}
		if app == "mysql" {
			steps = append([]Step{
				{Args: []string{"sh", "-c", mysqlCompatSetupScript}},
			}, steps...)
		}
		return steps, nil
	case "dnf", "yum":
		return []Step{
			{Args: []string{manager, "remove", "-y", pkg}},
			{Args: []string{manager, "autoremove", "-y"}, Optional: true},
		}, nil
	case "zypper":
		return []Step{{Args: []string{"zypper", "--non-interactive", "remove", pkg}}}, nil
	case "apk":
		return []Step{{Args: []string{"apk", "del", pkg}}}, nil
	}
	return nil, fmt.Errorf("unsupported package manager %q", manager)
}

// lookupAppPackage validates manager and app against the whitelists and
// returns the distro package name for the combination.
func lookupAppPackage(manager, app string) (string, error) {
	if !ValidPackageManager(manager) {
		return "", fmt.Errorf("unsupported package manager %q", manager)
	}
	spec, ok := AppSpecByName(app)
	if !ok {
		return "", fmt.Errorf("unknown app %q", app)
	}
	pkg, ok := spec.Packages[manager]
	if !ok || pkg == "" {
		return "", fmt.Errorf("app %s has no package for %s", app, manager)
	}
	return pkg, nil
}
