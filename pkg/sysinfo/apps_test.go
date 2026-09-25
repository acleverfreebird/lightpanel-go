package sysinfo

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
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
	for _, app := range []string{"nginx", "apache", "docker", "certbot"} {
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
	req := httptest.NewRequest("POST", "/api/apps/install", strings.NewReader(url.Values{"name": {"mysql"}}.Encode()))
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
	m := appsTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		ran <- struct{}{}
		<-block
		return "", errors.New("aborted")
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
	if len(steps) != 2 || steps[1][0] != "apt-get" || steps[1][1] != "install" || steps[1][3] != "nginx" {
		t.Fatalf("unexpected apt-get steps: %v", steps)
	}
}
