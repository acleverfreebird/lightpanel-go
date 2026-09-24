//go:build linux

package sysinfo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	updateAPITimeout  = 10 * time.Second
	updateDLTimeout   = 55 * time.Second // server WriteTimeout is 60s; keep the response deliverable
	maxUpdateAsset    = 64 << 20
	maxUpdateManifest = 64 << 10
)

// Configurable at startup from config; overridden directly in tests.
var (
	BuildVersion       = "dev"
	UpdateRepo         = "acleverfreebird/lightpanel-go"
	UpdateMirror       = ""
	updateAPIBase      = "https://api.github.com"
	updateDownloadBase = "https://github.com"
	unitName           = "lightpanel"
	executablePath     = os.Executable
	scheduleRestart    = defaultScheduleRestart
)

var (
	updateBusy   atomic.Bool
	errNoRelease = errors.New("no published release")
)

type releaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func githubFetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, updateAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "LightPanel/"+BuildVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", UpdateRepo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release lookup returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func fetchRelease(ctx context.Context, tag string) (*releaseInfo, error) {
	verb := "latest"
	if tag != "" {
		verb = "tags/" + tag
	}
	data, err := githubFetch(ctx, updateAPIBase+"/repos/"+UpdateRepo+"/releases/"+verb, 1<<20)
	if err != nil {
		return nil, err
	}
	var rel releaseInfo
	if err := json.Unmarshal(data, &rel); err != nil || rel.TagName == "" {
		return nil, fmt.Errorf("unexpected release payload")
	}
	return &rel, nil
}

// compareVersion returns >0 when the remote tag is newer than the local one.
// Tags are "vMAJOR.MINOR.PATCH" with an optional -suffix; anything unparseable
// falls back to string comparison.
func compareVersion(remote, local string) int {
	norm := func(v string) (parts []int, pre string, ok bool) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexByte(v, '-'); i >= 0 {
			pre, v = v[i+1:], v[:i]
		}
		for _, p := range strings.Split(v, ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, "", false
			}
			parts = append(parts, n)
		}
		return parts, pre, true
	}
	rp, rpre, rok := norm(remote)
	lp, lpre, lok := norm(local)
	if !rok || !lok {
		return strings.Compare(remote, local)
	}
	for i := 0; i < 3; i++ {
		r, l := 0, 0
		if i < len(rp) {
			r = rp[i]
		}
		if i < len(lp) {
			l = lp[i]
		}
		if r != l {
			return r - l
		}
	}
	// Same numeric version: a release beats a prerelease of that version.
	switch {
	case lpre != "" && rpre == "":
		return 1
	case rpre != "" && lpre == "":
		return -1
	default:
		return strings.Compare(rpre, lpre)
	}
}

type UpdateStatus struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Update  bool   `json:"update_available"`
	URL     string `json:"url"`
}

func UpdateCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := fetchRelease(r.Context(), "")
	if errors.Is(err, errNoRelease) {
		JSON(w, UpdateStatus{Current: BuildVersion, Latest: ""})
		return
	}
	if err != nil {
		commandError(w, "", err)
		return
	}
	JSON(w, UpdateStatus{Current: BuildVersion, Latest: rel.TagName, Update: compareVersion(rel.TagName, BuildVersion) > 0, URL: rel.HTMLURL})
}

func releaseAssetURL(tag, asset string) string {
	direct := updateDownloadBase + "/" + UpdateRepo + "/releases/download/" + tag + "/" + asset
	if UpdateMirror == "" {
		return direct
	}
	return UpdateMirror + "/" + direct
}

func downloadToFile(ctx context.Context, url string, dst *os.File, limit int64) error {
	ctx, cancel := context.WithTimeout(ctx, updateDLTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "LightPanel/"+BuildVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download of %s returned %d", filepath.Base(url), resp.StatusCode)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if n > limit {
		return fmt.Errorf("download exceeded %d MiB limit", limit>>20)
	}
	return nil
}

func verifyChecksum(dir, asset, target string) error {
	manifest, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == asset {
			expected = strings.ToLower(fields[0])
		}
	}
	if len(expected) != 64 {
		return fmt.Errorf("SHA256SUMS has no entry for %s", asset)
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err = io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, got)
	}
	return nil
}

func defaultScheduleRestart() {
	// Give the HTTP response time to flush, then restart from a new session so
	// the restart survives this process exiting.
	time.AfterFunc(1500*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		p, err := executable("systemctl")
		if err != nil {
			slog.Error("update_restart", "error", err.Error())
			return
		}
		sid := syscall.SysProcAttr{Setsid: true}
		c := exec.CommandContext(ctx, p, "restart", unitName)
		c.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat"}
		c.SysProcAttr = &sid
		if err = c.Start(); err != nil {
			slog.Error("update_restart", "error", err.Error())
			return
		}
		_ = c.Wait()
	})
}

func UpdateApply(w http.ResponseWriter, r *http.Request) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		http.Error(w, "no prebuilt release for this architecture", 501)
		return
	}
	if !updateBusy.CompareAndSwap(false, true) {
		http.Error(w, "an update is already in progress", 409)
		return
	}
	defer updateBusy.Store(false)
	applyUpdate(w, r)
}

func applyUpdate(w http.ResponseWriter, r *http.Request) {
	exe, err := executablePath()
	if err != nil {
		commandError(w, "", err)
		return
	}
	dir := filepath.Dir(exe)
	asset := "lightpanel-linux-" + runtime.GOARCH
	rel, err := fetchRelease(r.Context(), r.FormValue("tag"))
	if err != nil {
		commandError(w, "", err)
		return
	}
	if BuildVersion != "dev" && compareVersion(rel.TagName, BuildVersion) <= 0 {
		JSON(w, UpdateStatus{Current: BuildVersion, Latest: rel.TagName})
		return
	}
	// Download into the binary's own directory so the final rename is atomic
	// on the same filesystem and survives a crash mid-update.
	tmp, err := os.CreateTemp(dir, ".lightpanel-update-*")
	if err != nil {
		commandError(w, "", err)
		return
	}
	defer os.Remove(tmp.Name())
	if err = downloadToFile(r.Context(), releaseAssetURL(rel.TagName, asset), tmp, maxUpdateAsset); err != nil {
		commandError(w, "", err)
		return
	}
	if err = tmp.Close(); err != nil {
		commandError(w, "", err)
		return
	}
	manifest := filepath.Join(dir, "SHA256SUMS")
	mf, err := os.Create(manifest)
	if err != nil {
		commandError(w, "", err)
		return
	}
	if err = downloadToFile(r.Context(), releaseAssetURL(rel.TagName, "SHA256SUMS"), mf, maxUpdateManifest); err != nil {
		mf.Close()
		commandError(w, "", err)
		return
	}
	if err = mf.Close(); err != nil {
		commandError(w, "", err)
		return
	}
	if err = verifyChecksum(dir, asset, tmp.Name()); err != nil {
		commandError(w, "", err)
		return
	}
	if err = os.Chmod(tmp.Name(), 0755); err != nil {
		commandError(w, "", err)
		return
	}
	if err = os.Rename(tmp.Name(), exe); err != nil {
		commandError(w, "", err)
		return
	}
	slog.Info("update_applied", "from", BuildVersion, "to", rel.TagName)
	scheduleRestart()
	JSON(w, UpdateStatus{Current: rel.TagName, Latest: rel.TagName, Update: false})
}
