package sysinfo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"lightpanel/pkg/helper"
)

func TestServicesIncludesUnloadedUnitFiles(t *testing.T) {
	loaded := "ssh.service loaded active running Secure Shell\n● broken.service loaded failed failed Broken worker\n"
	m := &Manager{Run: func(_ context.Context, command string, args ...string) (string, error) {
		if command != "systemctl" {
			t.Fatalf("unexpected command %s", command)
		}
		if args[0] == "list-unit-files" {
			return "ssh.service enabled enabled\nbackup.service disabled enabled\nbroken.service static -\n", nil
		}
		return loaded, nil
	}}
	w := httptest.NewRecorder()
	m.Services(w, httptest.NewRequest("GET", "/api/services", nil))
	var got struct {
		Output string `json:"output"`
		Items  []struct {
			Name, State, Description string
			UnitFileState            string `json:"unit_file_state"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Output != loaded {
		t.Fatalf("legacy output changed: %q", got.Output)
	}
	if len(got.Items) != 3 {
		t.Fatalf("want 3 loaded and installed services, got %+v", got.Items)
	}
	if got.Items[0].Name != "backup.service" || got.Items[0].State != "inactive" || got.Items[0].UnitFileState != "disabled" {
		t.Fatalf("missing unloaded service: %+v", got.Items[0])
	}
	if got.Items[1].State != "failed" || got.Items[2].Description != "Secure Shell" || got.Items[2].UnitFileState != "enabled" {
		t.Fatalf("wrong merged states: %+v", got.Items)
	}
}

func TestServicesUnitFileFailureIsReported(t *testing.T) {
	m := &Manager{Run: func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "list-unit-files" {
			return "permission denied", errors.New("failed")
		}
		return "", nil
	}}
	w := httptest.NewRecorder()
	m.Services(w, httptest.NewRequest("GET", "/api/services", nil))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "permission denied") {
		t.Fatalf("failure lost: %d %s", w.Code, w.Body.String())
	}
}

func TestServiceAdditionalActions(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	for _, action := range []string{"enable", "disable", "reload"} {
		t.Run(action, func(t *testing.T) {
			PrivilegedCall = nil
			var got []string
			m := &Manager{Run: func(_ context.Context, command string, args ...string) (string, error) {
				got = append([]string{command}, args...)
				return "", nil
			}}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/api/service/action", strings.NewReader(url.Values{"name": {"ssh.service"}, "action": {action}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				m.ServiceAction(w, r)
				return w
			}
			if w := request(); w.Code != 200 {
				t.Fatalf("action rejected: %d %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(got, []string{"systemctl", "--no-ask-password", action, "--", "ssh.service"}) {
				t.Fatalf("unsafe command: %v", got)
			}
			got = nil
			PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
				if req.Op != helper.OpService || req.Unit != "ssh.service" || req.Action != action {
					t.Fatalf("wrong helper request: %+v", req)
				}
				return "", errors.New("denied by acl")
			}
			if w := request(); w.Code != 502 || len(got) != 0 {
				t.Fatalf("helper denial bypassed: %d %v", w.Code, got)
			}
		})
	}
}
