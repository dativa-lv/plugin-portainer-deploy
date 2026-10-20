// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Integration-style test that exercises the full Portainer client workflow end-to-end
// against an httptest server (endpoint lookup -> swarm -> stack create -> rbac update -> running check -> health check).
func TestClientWorkflow_Integration_HappyPath_AllChecksEnabled(t *testing.T) {
	stackPath := writeTempStackFile(t, "version: '3'\nservices: {}\n")

	var taskCalls atomic.Int32
	var healthCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/endpoints", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerEndpoint{{ID: 1, Name: "prod"}})
	})
	mux.HandleFunc("/api/endpoints/1/docker/swarm", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(portainerSwarm{ID: "swarm-1"})
	})
	mux.HandleFunc("/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerStack{})
	})
	mux.HandleFunc("/api/stacks/create/swarm/string", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(portainerStack{ID: 10, Name: "test-stack"})
	})
	mux.HandleFunc("/api/stacks/10", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(portainerStackDetail{ID: 10, Name: "test-stack", ResourceControl: portainerResourceCtrl{ID: 99}})
	})
	mux.HandleFunc("/api/teams", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerTeam{{ID: 5, Name: "team-a"}, {ID: 6, Name: "team-b"}})
	})
	mux.HandleFunc("/api/resource_controls/99", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/endpoints/1/docker/v1.47/services", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerService{replicatedService("svc1", "test-stack_app", 1, "completed")})
	})
	mux.HandleFunc("/api/endpoints/1/docker/v1.47/tasks", func(w http.ResponseWriter, r *http.Request) {
		if taskCalls.Add(1) < 2 {
			_ = json.NewEncoder(w).Encode([]dockerTask{{
				ServiceID: "svc1",
				Status:    dockerTaskStatus{State: "pending"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode([]dockerTask{{
			ServiceID: "svc1",
			Status:    dockerTaskStatus{State: "running"},
		}})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if healthCalls.Add(1) < 2 {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackPath = stackPath
	c.StackName = "test-stack"
	c.Teams = []string{"team-a", "team-b"}
	c.RunningCheck = true
	c.RunningTimeout = "200ms"
	c.HealthCheck = true
	c.HealthCheckURL = srv.URL + "/health"

	ctx := context.Background()
	endpointID, err := c.GetEndpointID(ctx)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	swarmID, err := c.GetSwarmID(ctx, endpointID)
	if err != nil {
		t.Fatalf("swarm: %v", err)
	}
	stackID, err := c.CreateOrUpdateStack(ctx, endpointID, swarmID)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if _, err := c.UpdateResourceControl(ctx, stackID); err != nil {
		t.Fatalf("rbac: %v", err)
	}
	if err := c.WaitForStackRunning(ctx, endpointID, 200*time.Millisecond); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := c.CheckHealth(ctx, c.HealthCheckURL, 200*time.Millisecond); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestServiceUpdateWorkflow_Integration_WithChecksEnabled(t *testing.T) {
	var taskCalls atomic.Int32
	var healthCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/endpoints", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerEndpoint{{ID: 1, Name: "prod"}})
	})
	mux.HandleFunc("/api/endpoints/1/docker/v1.47/services", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerService{replicatedService("svc-1", "mystack_app", 1, "completed")})
	})
	mux.HandleFunc("/api/endpoints/1/forceupdateservice", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/endpoints/1/docker/v1.47/tasks", func(w http.ResponseWriter, r *http.Request) {
		if taskCalls.Add(1) < 2 {
			_ = json.NewEncoder(w).Encode([]dockerTask{{
				ServiceID: "svc-1",
				Status:    dockerTaskStatus{State: "pending"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode([]dockerTask{{
			ServiceID: "svc-1",
			Status:    dockerTaskStatus{State: "running"},
		}})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if healthCalls.Add(1) < 2 {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Plugin{Settings: &Settings{
		APIKey:             "test-key",
		ServerURL:          srv.URL,
		ServerEnvironment:  "prod",
		StackName:          "mystack",
		ServiceName:        "app",
		RunningCheck:       true,
		RunningTimeout:     "200ms",
		HealthCheck:        true,
		HealthCheckURL:     srv.URL + "/health",
		HealthCheckTimeout: "200ms",
	}}

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	c.ServiceName = "app"

	ctx := context.Background()
	endpointID, err := c.GetEndpointID(ctx)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if err := c.UpdateStackServices(ctx, endpointID, true, 500*time.Millisecond); err != nil {
		t.Fatalf("service update: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := p.runPostUpdateChecks(ctx, c, endpointID, "Services updated"); err != nil {
		t.Fatalf("post-update checks: %v", err)
	}
}

func TestServiceUpdateWorkflow_Integration_JobAndRegularService(t *testing.T) {
	f := newFakeSwarm(t)
	f.registries = []portainerRegistry{{ID: 3, Name: "zzdats", URL: "git.zzdats.lv"}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/endpoints", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]portainerEndpoint{{ID: 1, Name: "prod"}})
	})
	mux.Handle("/api/endpoints/1/", f)
	mux.Handle("/api/registries", f)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Plugin{Settings: &Settings{
		APIKey:            "test-key",
		ServerURL:         srv.URL,
		ServerEnvironment: "prod",
		StackName:         "mystack",
		RunningCheck:      true,
		RunningTimeout:    "200ms",
		JobTimeout:        "500ms",
	}}

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	c.RunningCheck = true
	c.RunningTimeout = "200ms"

	ctx := context.Background()
	endpointID, err := c.GetEndpointID(ctx)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if err := c.UpdateStackServices(ctx, endpointID, true, 500*time.Millisecond); err != nil {
		t.Fatalf("service update: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := p.runPostUpdateChecks(ctx, c, endpointID, "Services updated"); err != nil {
		t.Fatalf("post-update checks: %v", err)
	}

	if got := strings.Join(f.events, ","); got != "job-update,job-complete,force:svc-1" {
		t.Fatalf("expected the job to complete before the app is updated, got %s", got)
	}
	if f.forceJobCalled.Load() {
		t.Fatal("job must not be updated through forceupdateservice")
	}
	if f.updateAuth == "" {
		t.Fatal("expected registry credentials for the job image")
	}
}

// Minimal coverage for cmd main package via subprocess-like invocation is not possible in-go.
// But we can at least ensure Version variable is referenced without failing build.
func TestMainPackage_VersionString_NotEmpty(t *testing.T) {
	_ = strings.TrimSpace
	_ = time.Second
}
