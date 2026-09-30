package sysinfo

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"lightpanel/pkg/helper"
)

func appsTestManager(run Runner) *AppManager {
	timeout := func(ctx context.Context, _ time.Duration, name string, args ...string) (string, error) {
		return run(ctx, name, args...)
	}
	return &AppManager{SiteManager: SiteManager{Run: run, RunTimeout: timeout}}
}

func TestAppsHandlerDetectsNothingWithoutTools(t *testing.T) {
	m := appsTestManager(func(context.Context, string, ...string) (string, error) { return "", ErrUnavailable })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/apps", nil)
	m.Apps(rec, req)
	if rec.Code != 200 {
		t.Fatalf("Apps status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"package_manager":""`) {
		t.Errorf("expected empty package manager, got: %s", body)
	}
	// The whole catalog stays visible so the store can render install targets.
	for _, app := range []string{"nginx", "apache", "docker", "mysql", "mariadb", "postgresql", "redis"} {
		if !strings.Contains(body, `"name":"`+app+`"`) {
			t.Errorf("catalog missing app %q, got: %s", app, body)
		}
	}
	if strings.Count(body, `"installed":true`) != 0 {
		t.Errorf("no app should be installed when every probe fails: %s", body)
	}
}

func TestAppInstallRejectsUnknownApp(t *testing.T) {
	m := appsTestManager(func(context.Context, string, ...string) (string, error) { return "", ErrUnavailable })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/apps/install", strings.NewReader(url.Values{"name": {"mysql8"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.AppInstall(rec, req)
	if rec.Code != 400 {
		t.Fatalf("unknown app status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "nginx") {
		t.Errorf("error must not leak catalog entries: %s", rec.Body.String())
	}
}

func TestAppInstallSingleSlot(t *testing.T) {
	block := make(chan struct{})
	ran := make(chan struct{}, 1)
	// Answer detection probes fast, park only on the real apt-get install
	// step — the handler now probes installed state synchronously, so a stub
	// that parks every call would deadlock its own conflict check.
	m := appsTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "apt-get" && slices.Contains(args, "install") {
			ran <- struct{}{}
			<-block
			return "", errors.New("aborted")
		}
		if len(args) > 0 && args[0] == "--version" {
			return "stub", nil
		}
		return "", ErrUnavailable
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/apps/install", strings.NewReader(url.Values{"name": {"nginx"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.AppInstall(rec, req)
	if rec.Code != 200 {
		t.Fatalf("first install status = %d, want 200", rec.Code)
	}
	<-ran // the job reached the runner and is now parked
	rec2 := httptest.NewRecorder()
	m.AppInstall(rec2, req)
	if rec2.Code != 409 {
		t.Fatalf("second concurrent install status = %d, want 409", rec2.Code)
	}
	close(block)
}

func TestAppInstallStepsRoundTrip(t *testing.T) {
	// The panel executes exactly what the shared catalog builds; assert one
	// representative manager so a helper-side change breaks this test too.
	steps, err := helper.AppInstallSteps("apt-get", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 ||
		steps[2].Args[0] != "apt-get" || steps[2].Args[3] != "install" || steps[2].Args[5] != "nginx" ||
		!steps[0].Optional || !steps[1].Optional || steps[2].Optional {
		t.Fatalf("unexpected apt-get steps: %v", steps)
	}
}

func TestAppInstallBlockedByGroupConflict(t *testing.T) {
	// Apache answers its probe; nginx does not. Installing nginx must be
	// rejected because the two web servers share one catalog group.
	m := appsTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "apache2ctl" {
			return "Server version: Apache/2.4.62", nil
		}
		return "", ErrUnavailable
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/apps/install", strings.NewReader(url.Values{"name": {"nginx"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.AppInstall(rec, req)
	if rec.Code != 409 {
		t.Fatalf("install blocked by group conflict status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Apache") {
		t.Errorf("conflict message should name the installed blocker: %s", rec.Body.String())
	}
}

func TestAppRemoveRejectsUnknownApp(t *testing.T) {
	m := appsTestManager(func(context.Context, string, ...string) (string, error) { return "", ErrUnavailable })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/apps/remove", strings.NewReader(url.Values{"name": {"nginx; rm -rf /"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.AppRemove(rec, req)
	if rec.Code != 400 {
		t.Fatalf("unknown app remove status = %d, want 400", rec.Code)
	}
}

func TestAppRemoveRunsAsTask(t *testing.T) {
	block := make(chan struct{})
	ran := make(chan struct{}, 1)
	// Like TestAppInstallSingleSlot: detection probes answer fast, only the
	// real apt-get purge step parks.
	m := appsTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "apt-get" && slices.Contains(args, "purge") {
			ran <- struct{}{}
			<-block
			return "purged", nil
		}
		if len(args) > 0 && args[0] == "--version" {
			return "stub", nil
		}
		return "", ErrUnavailable
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/apps/remove", strings.NewReader(url.Values{"name": {"nginx"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.AppRemove(rec, req)
	if rec.Code != 200 {
		t.Fatalf("remove status = %d, want 200", rec.Code)
	}
	<-ran // the job reached the runner and is now parked
	// While the remove job is parked in the registry, both another remove and
	// an install of the same app must be rejected.
	req2 := httptest.NewRequest("POST", "/api/apps/remove", strings.NewReader(url.Values{"name": {"nginx"}}.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	m.AppRemove(rec2, req2)
	if rec2.Code != 409 {
		t.Fatalf("concurrent remove status = %d, want 409", rec2.Code)
	}
	req3 := httptest.NewRequest("POST", "/api/apps/install", strings.NewReader(url.Values{"name": {"nginx"}}.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec3 := httptest.NewRecorder()
	m.AppInstall(rec3, req3)
	if rec3.Code != 409 {
		t.Fatalf("concurrent install status = %d, want 409", rec3.Code)
	}
	close(block)
}

func TestAppRemoveStepsRoundTrip(t *testing.T) {
	steps, err := helper.AppRemoveSteps("apt-get", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 ||
		steps[1].Args[0] != "apt-get" || steps[1].Args[3] != "purge" || steps[1].Args[5] != "nginx" ||
		steps[1].Optional || !steps[2].Optional {
		t.Fatalf("unexpected apt-get remove steps: %v", steps)
	}
}
