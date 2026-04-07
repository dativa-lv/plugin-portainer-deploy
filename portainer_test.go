// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient builds a minimal Client wired to the given HTTP client.
// plugin is nil because none of the refactored methods call c.plugin.* directly.
func newTestClient(t *testing.T, hc *http.Client, serverURL, env string) *Client {
	t.Helper()
	return &Client{
		httpClient:        hc,
		pollInterval:      10 * time.Millisecond,
		ServerURL:         serverURL,
		ServerEnvironment: env,
		StackName:         "test-stack",
		APIKey:            "test-key",
	}
}

// --- GetEndpointID ---

func TestGetEndpointID_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]portainerEndpoint{
			{ID: 42, Name: "prod"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetEndpointID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 42 {
		t.Errorf("expected ID 42, got %d", id)
	}
}

func TestGetEndpointID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]portainerEndpoint{
			{ID: 1, Name: "other"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetEndpointID(context.Background())
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
}

func TestGetEndpointID_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetEndpointID(context.Background())
	if err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestGetEndpointID_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetEndpointID(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestGetEndpointID_NoType1Filter(t *testing.T) {
	var requestURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURL = r.URL.String()
		json.NewEncoder(w).Encode([]portainerEndpoint{{ID: 1, Name: "prod"}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.GetEndpointID(context.Background())

	sub := "type=1"
	if containsString(requestURL, sub) {
		t.Errorf("request URL should not contain %q (BUG-07), got: %s", sub, requestURL)
	}
}

// --- GetSwarmID ---

func TestGetSwarmID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(portainerSwarm{ID: "swarm-abc"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetSwarmID(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "swarm-abc" {
		t.Errorf("expected swarm-abc, got %s", id)
	}
}

func TestGetSwarmID_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetSwarmID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestGetSwarmID_EmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(portainerSwarm{ID: ""})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetSwarmID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error when Swarm ID is empty")
	}
}

// --- GetTeamIDByName ---

func TestGetTeamIDByName_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]portainerTeam{
			{ID: 7, Name: "devteam"},
			{ID: 8, Name: "other"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetTeamIDByName(context.Background(), "devteam")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "7" {
		t.Errorf("expected 7, got %s", id)
	}
}

func TestGetTeamIDByName_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]portainerTeam{{ID: 1, Name: "other"}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetTeamIDByName(context.Background(), "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty string for missing team, got %q", id)
	}
}

func TestGetTeamIDByName_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetTeamIDByName(context.Background(), "devteam")
	if err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestGetTeamIDByName_TrimsWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]portainerTeam{{ID: 3, Name: "myteam"}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetTeamIDByName(context.Background(), "  myteam  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "3" {
		t.Errorf("expected 3, got %s", id)
	}
}

// --- GetResourceID ---

func TestGetResourceID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(portainerStackDetail{
			ID:   10,
			Name: "test-stack",
			ResourceControl: portainerResourceCtrl{
				ID: 99,
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	id, err := c.GetResourceID(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "99" {
		t.Errorf("expected 99, got %s", id)
	}
}

func TestGetResourceID_MissingResourceControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ResourceControl.ID defaults to 0 — should be treated as missing
		json.NewEncoder(w).Encode(portainerStackDetail{ID: 10, Name: "test-stack"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetResourceID(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error when ResourceControl is absent")
	}
}

func TestGetResourceID_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetResourceID(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

// --- ConvertToIntSlice ---

func TestConvertToIntSlice_ValidInput(t *testing.T) {
	result := ConvertToIntSlice([]string{"1", "2", "3"})
	if len(result) != 3 || result[0] != 1 || result[1] != 2 || result[2] != 3 {
		t.Errorf("unexpected result: %v", result)
	}
}

func TestConvertToIntSlice_SkipsInvalidEntries(t *testing.T) {
	result := ConvertToIntSlice([]string{"1", "abc", "3"})
	if len(result) != 2 {
		t.Fatalf("expected 2 valid entries, got %d: %v", len(result), result)
	}
	if result[0] != 1 || result[1] != 3 {
		t.Errorf("unexpected values: %v", result)
	}
}

func TestConvertToIntSlice_EmptyInput(t *testing.T) {
	result := ConvertToIntSlice([]string{})
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestConvertToIntSlice_TrailingComma(t *testing.T) {
	result := ConvertToIntSlice([]string{"5,"})
	if len(result) != 1 || result[0] != 5 {
		t.Errorf("expected [5], got %v", result)
	}
}

// --- Context cancellation ---

func TestGetEndpointID_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler is never reached because context is already cancelled.
		json.NewEncoder(w).Encode([]portainerEndpoint{{ID: 1, Name: "prod"}})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	_, err := c.GetEndpointID(ctx)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

// containsString is a simple helper to avoid importing strings in tests.
func containsString(s, sub string) bool { return strings.Contains(s, sub) }

func writeTempStackFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "stack.yml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp stack file: %v", err)
	}
	return p
}

// --- ConvertToIntSlice ---

func TestConvertToIntSlice_SkipsInvalid(t *testing.T) {
	got := ConvertToIntSlice([]string{"1", " 2 ", "bad", "3,"})
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("unexpected result: %#v", got)
	}
}

// --- CreateOrUpdateStack / CreateNewStack / UpdateExistingStack ---

func TestCreateOrUpdateStack_UpdatesWhenExists(t *testing.T) {
	stackPath := writeTempStackFile(t, "version: '3'\nservices: {}\n")

	var putCalled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/stacks"):
			_ = json.NewEncoder(w).Encode([]portainerStack{{ID: 10, Name: "test-stack"}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/10":
			putCalled.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte("StackFileContent")) {
				t.Fatalf("expected StackFileContent in update payload, got: %s", string(body))
			}
			_ = json.NewEncoder(w).Encode(portainerStack{ID: 10, Name: "test-stack"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackPath = stackPath
	id, err := c.CreateOrUpdateStack(context.Background(), 1, "swarm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 10 {
		t.Fatalf("expected stack id 10, got %d", id)
	}
	if putCalled.Load() != 1 {
		t.Fatalf("expected UpdateExistingStack to be called once, got %d", putCalled.Load())
	}
}

func TestCreateOrUpdateStack_CreatesWhenMissing(t *testing.T) {
	stackPath := writeTempStackFile(t, "version: '3'\nservices: {}\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/stacks"):
			_ = json.NewEncoder(w).Encode([]portainerStack{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/stacks/create/swarm/string":
			_ = json.NewEncoder(w).Encode(portainerStack{ID: 55, Name: "test-stack"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackPath = stackPath
	id, err := c.CreateOrUpdateStack(context.Background(), 1, "swarm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 55 {
		t.Fatalf("expected stack id 55, got %d", id)
	}
}

func TestCreateNewStack_NonOK(t *testing.T) {
	stackPath := writeTempStackFile(t, "version: '3'\nservices: {}\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackPath = stackPath
	_, err := c.CreateNewStack(context.Background(), 1, "swarm")
	if err == nil {
		t.Fatal("expected error for non-OK create")
	}
}

func TestUpdateExistingStack_NonOK(t *testing.T) {
	stackPath := writeTempStackFile(t, "version: '3'\nservices: {}\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackPath = stackPath
	_, err := c.UpdateExistingStack(context.Background(), 10, 1)
	if err == nil {
		t.Fatal("expected error for non-OK update")
	}
}

// --- UpdateResourceControl ---

func TestUpdateResourceControl_SetsAdminOnlyWhenNoTeams(t *testing.T) {
	var seenPutBody []byte
	calls := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/10":
			_ = json.NewEncoder(w).Encode(portainerStackDetail{ID: 10, Name: "test-stack", ResourceControl: portainerResourceCtrl{ID: 99}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/resource_controls/99":
			seenPutBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.Teams = nil
	_, err := c.UpdateResourceControl(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls["GET /api/stacks/10"] != 1 || calls["PUT /api/resource_controls/99"] != 1 {
		t.Fatalf("unexpected calls: %#v", calls)
	}

	if !bytes.Contains(seenPutBody, []byte(`"administratorsOnly":true`)) {
		t.Fatalf("expected administratorsOnly=true, body=%s", string(seenPutBody))
	}
}

func TestUpdateResourceControl_RestrictedWhenTeamsProvided(t *testing.T) {
	var seenPutBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/10":
			_ = json.NewEncoder(w).Encode(portainerStackDetail{ID: 10, Name: "test-stack", ResourceControl: portainerResourceCtrl{ID: 77}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/teams":
			_ = json.NewEncoder(w).Encode([]portainerTeam{{ID: 5, Name: "team-a"}, {ID: 6, Name: "team-b"}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/resource_controls/77":
			seenPutBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.Teams = []string{"team-a", "team-b"}
	_, err := c.UpdateResourceControl(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(seenPutBody, []byte(`"restricted":true`)) {
		t.Fatalf("expected restricted=true in body: %s", string(seenPutBody))
	}
	if !bytes.Contains(seenPutBody, []byte(`"teams":[5,6]`)) {
		t.Fatalf("expected teams [5,6] in body: %s", string(seenPutBody))
	}
}

// --- WaitForStackRunning ---

func TestWaitForStackRunning_EventuallyRunning(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := calls.Add(1)
		if c < 2 {
			_ = json.NewEncoder(w).Encode([]dockerTask{{Status: struct {
				State string `json:"State"`
			}{State: "pending"}}})
			return
		}
		_ = json.NewEncoder(w).Encode([]dockerTask{{Status: struct {
			State string `json:"State"`
		}{State: "running"}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	if err := c.WaitForStackRunning(context.Background(), 1, 200*time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStackRunning_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]dockerTask{{Status: struct {
			State string `json:"State"`
		}{State: "pending"}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	if err := c.WaitForStackRunning(context.Background(), 1, 30*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}

// --- CheckHealth ---

func TestCheckHealth_SucceedsOn2xx(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 2 {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.CheckHealth(context.Background(), srv.URL, 200*time.Millisecond); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestCheckHealth_HealthJSONFailTreatsUnhealthy(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 2 {
			w.Header().Set("Content-Type", "application/health+json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"fail"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.CheckHealth(context.Background(), srv.URL, 200*time.Millisecond); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
}

func TestCheckHealth_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.CheckHealth(context.Background(), srv.URL, 30*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}
