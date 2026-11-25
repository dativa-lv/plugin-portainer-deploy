// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeDocker = "/api/endpoints/1/docker/v1.47"

type fakeConfig struct {
	ID     string
	Name   string
	Labels map[string]string
	Data   string
	RCID   int
}

type configRef struct {
	ID   string
	Name string
}

type fakeConfigService struct {
	Name      string
	Namespace string
	Refs      []configRef
}

// fakeConfigPortainer is a minimal Portainer API for configs mode.
type fakeConfigPortainer struct {
	t              *testing.T
	server         *httptest.Server
	mu             sync.Mutex
	calls          []string
	configs        map[string]*fakeConfig
	stacks         map[int]*portainerStack
	services       []fakeConfigService
	teams          []portainerTeam
	rcUpdates      map[int]ResourceControl
	deleteFailures map[string]int
	deleteStatus   int
	createFailures map[string]int
	createNoRC     bool
	stopConflict   bool
	startConflict  bool
	tasksRunning   bool
	nextConfig     int
	nextRC         int
}

func newFakeConfigPortainer(t *testing.T) *fakeConfigPortainer {
	f := &fakeConfigPortainer{
		t:              t,
		configs:        map[string]*fakeConfig{},
		stacks:         map[int]*portainerStack{},
		teams:          []portainerTeam{{ID: 3, Name: "DevOps"}, {ID: 7, Name: "QA"}},
		rcUpdates:      map[int]ResourceControl{},
		deleteFailures: map[string]int{},
		createFailures: map[string]int{},
		tasksRunning:   true,
		nextConfig:     100,
		nextRC:         500,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeConfigPortainer) addConfig(id, name string, data string, labels map[string]string, rcID int) {
	f.configs[id] = &fakeConfig{ID: id, Name: name, Labels: labels, Data: base64.StdEncoding.EncodeToString([]byte(data)), RCID: rcID}
}

func (f *fakeConfigPortainer) addStack(id int, name string, status int) {
	f.stacks[id] = &portainerStack{ID: id, Name: name, EndpointID: 1, Status: status}
}

func (f *fakeConfigPortainer) addService(name, namespace string, refs ...configRef) {
	f.services = append(f.services, fakeConfigService{Name: name, Namespace: namespace, Refs: refs})
}

func (f *fakeConfigPortainer) configByName(name string) *fakeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cfg := range f.configs {
		if cfg.Name == name {
			return cfg
		}
	}
	return nil
}

func (f *fakeConfigPortainer) callIndex(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	return -1
}

func (f *fakeConfigPortainer) countCalls(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeConfigPortainer) mutatingCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, http.MethodGet+" ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeConfigPortainer) configJSON(cfg *fakeConfig) map[string]any {
	out := map[string]any{
		"ID":   cfg.ID,
		"Spec": map[string]any{"Name": cfg.Name, "Labels": cfg.Labels, "Data": cfg.Data},
	}
	if cfg.RCID != 0 {
		out["Portainer"] = map[string]any{"ResourceControl": map[string]any{"Id": cfg.RCID}}
	}
	return out
}

func (f *fakeConfigPortainer) serviceJSON(svc fakeConfigService) map[string]any {
	refs := []map[string]any{}
	for _, ref := range svc.Refs {
		refs = append(refs, map[string]any{"ConfigID": ref.ID, "ConfigName": ref.Name})
	}
	labels := map[string]string{}
	if svc.Namespace != "" {
		labels[stackNamespaceLabel] = svc.Namespace
	}
	return map[string]any{
		"ID": svc.Name,
		"Spec": map[string]any{
			"Name":         svc.Name,
			"Labels":       labels,
			"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 1}},
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Configs": refs}},
		},
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set(contentTypeHeader, contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// namespaceFilter returns the stack namespace from a Docker label filter, or "" without one.
func namespaceFilter(r *http.Request) string {
	var filters struct {
		Label []string `json:"label"`
	}
	_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
	for _, label := range filters.Label {
		if namespace, ok := strings.CutPrefix(label, stackNamespaceLabel+"="); ok {
			return namespace
		}
	}
	return ""
}

func (f *fakeConfigPortainer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	path := r.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")

	switch {
	case r.Method == http.MethodGet && path == "/api/endpoints":
		writeFakeJSON(w, http.StatusOK, []portainerEndpoint{{ID: 1, Name: "primary"}, {ID: 2, Name: "primary-dr"}})
	case r.Method == http.MethodGet && path == "/api/teams":
		writeFakeJSON(w, http.StatusOK, f.teams)
	case r.Method == http.MethodGet && path == "/api/stacks":
		out := []portainerStack{}
		for _, stack := range f.stacks {
			out = append(out, *stack)
		}
		writeFakeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "stacks" && (parts[3] == "stop" || parts[3] == "start"):
		if r.URL.Query().Get("endpointId") != "1" {
			f.t.Errorf("stack %s without endpointId: %s", parts[3], r.URL)
		}
		id, _ := strconv.Atoi(parts[2])
		stack := f.stacks[id]
		wantActive := parts[3] == "stop"
		if stack == nil || (parts[3] == "stop" && f.stopConflict) || (parts[3] == "start" && f.startConflict) ||
			(stack.Status == stackStatusActive) != wantActive {
			writeFakeJSON(w, http.StatusConflict, map[string]any{"message": "conflict"})
			return
		}
		if wantActive {
			stack.Status = 2
		} else {
			stack.Status = stackStatusActive
		}
		writeFakeJSON(w, http.StatusOK, stack)
	case r.Method == http.MethodGet && path == fakeDocker+"/configs":
		var filters struct {
			Name []string `json:"name"`
		}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		out := []map[string]any{}
		for _, cfg := range f.configs {
			if len(filters.Name) == 0 || strings.HasPrefix(cfg.Name, filters.Name[0]) {
				out = append(out, f.configJSON(cfg))
			}
		}
		writeFakeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPost && path == fakeDocker+"/configs/create":
		var spec struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
			Data   string            `json:"Data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		if f.createFailures[spec.Name] > 0 {
			f.createFailures[spec.Name]--
			writeFakeJSON(w, http.StatusInternalServerError, map[string]any{"message": "create failed"})
			return
		}
		f.nextConfig++
		f.nextRC++
		id := "cfg-" + strconv.Itoa(f.nextConfig)
		f.configs[id] = &fakeConfig{ID: id, Name: spec.Name, Labels: spec.Labels, Data: spec.Data, RCID: f.nextRC}
		created := map[string]any{"ID": id}
		if !f.createNoRC {
			created["Portainer"] = map[string]any{"ResourceControl": map[string]any{"Id": f.nextRC}}
		}
		writeFakeJSON(w, http.StatusCreated, created)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, fakeDocker+"/configs/"):
		if r.ContentLength > 0 {
			f.t.Errorf("DELETE with a body: %s", path)
		}
		id := parts[len(parts)-1]
		if f.deleteStatus != 0 {
			writeFakeJSON(w, f.deleteStatus, map[string]any{"message": "delete failed"})
			return
		}
		if f.deleteFailures[id] > 0 {
			f.deleteFailures[id]--
			// Docker answers 400 while a service still uses the config.
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"message": "config is in use by the following service"})
			return
		}
		if _, ok := f.configs[id]; !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"message": "config not found"})
			return
		}
		delete(f.configs, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == fakeDocker+"/services":
		namespace := namespaceFilter(r)
		out := []map[string]any{}
		for _, svc := range f.services {
			if namespace == "" || svc.Namespace == namespace {
				out = append(out, f.serviceJSON(svc))
			}
		}
		writeFakeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == fakeDocker+"/tasks":
		namespace := namespaceFilter(r)
		state := "starting"
		if f.tasksRunning {
			state = "running"
		}
		out := []map[string]any{}
		for _, svc := range f.services {
			if svc.Namespace == namespace {
				out = append(out, map[string]any{"ServiceID": svc.Name, "Status": map[string]any{"State": state}})
			}
		}
		writeFakeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPut && len(parts) == 3 && parts[1] == "resource_controls":
		id, _ := strconv.Atoi(parts[2])
		var body ResourceControl
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.rcUpdates[id] = body
		writeFakeJSON(w, http.StatusOK, map[string]any{"Id": id})
	case r.Method == http.MethodPost && path == "/api/resource_controls":
		var body struct {
			ResourceID string `json:"resourceID"`
			Type       int    `json:"type"`
			ResourceControl
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Type != configResourceType {
			f.t.Errorf("resource control type %d, want %d", body.Type, configResourceType)
		}
		if cfg, ok := f.configs[body.ResourceID]; ok && cfg.RCID != 0 {
			writeFakeJSON(w, http.StatusConflict, map[string]any{"message": "resource control already exists"})
			return
		}
		f.nextRC++
		if cfg, ok := f.configs[body.ResourceID]; ok {
			cfg.RCID = f.nextRC
		}
		f.rcUpdates[f.nextRC] = body.ResourceControl
		writeFakeJSON(w, http.StatusOK, map[string]any{"Id": f.nextRC})
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, path)
		writeFakeJSON(w, http.StatusInternalServerError, map[string]any{"message": "unexpected request"})
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.dat")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configSettings(serverURL string, configs []ConfigEntry, teams string) *Settings {
	raw, _ := json.Marshal(configs)
	return &Settings{
		ServerURL:         serverURL,
		APIKey:            "test-key",
		ServerEnvironment: "primary",
		ConfigsJSON:       string(raw),
		Teams:             teams,
		RunningCheck:      false,
		RunningTimeout:    "1m",
	}
}

// runConfigs validates the settings and syncs the configs with a fast test client.
func runConfigs(t *testing.T, f *fakeConfigPortainer, s *Settings) error {
	t.Helper()
	p := &Plugin{Settings: s}
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	client := newTestClient(t, f.server.Client(), f.server.URL, s.ServerEnvironment)
	client.StackName = ""
	client.retryDelay = time.Millisecond
	client.Public = s.Public
	for team := range strings.SplitSeq(s.Teams, ",") {
		if team = strings.TrimSpace(team); team != "" {
			client.Teams = append(client.Teams, team)
		}
	}
	return p.syncConfigs(context.Background(), client)
}

func TestValidateConfigs(t *testing.T) {
	cases := []struct {
		name    string
		configs string
		modify  func(*Settings)
		wantErr string
	}{
		{"invalid json", "not-json", nil, "configs must be a list"},
		{"empty list", "[]", nil, "at least one entry"},
		{"missing file", `[{"name":"a"}]`, nil, "must have both name and file"},
		{"missing name", `[{"file":"a"}]`, nil, "must have both name and file"},
		{"duplicate names", `[{"name":"a","file":"x"},{"name":"a","file":"y"}]`, nil, "duplicate config name"},
		{"with stack name", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.StackName = "app" }, "cannot be combined"},
		{"with stack path", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.StackPath = "app.yml" }, "cannot be combined"},
		{"with health check", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.HealthCheck = true }, "cannot be combined"},
		{"with images check", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.ImagesCheck = true }, "images-check and configs"},
		{"public with teams", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.Public = true; s.Teams = "QA" }, "public and teams"},
		{"bad running timeout", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.RunningCheck = true; s.RunningTimeout = "soon" }, "running-check-timeout"},
		{"missing api key", `[{"name":"a","file":"x"}]`, func(s *Settings) { s.APIKey = "" }, "API key"},
		{"valid without stack name", `[{"name":"a","file":"x"}]`, nil, ""},
		{"valid uppercase keys", `[{"Name":"a","File":"x"}]`, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Settings{
				ServerURL:         "http://portainer",
				APIKey:            "key",
				ServerEnvironment: "primary",
				ConfigsJSON:       tc.configs,
				RunningCheck:      true,
				RunningTimeout:    "1m",
			}
			if tc.modify != nil {
				tc.modify(s)
			}
			err := (&Plugin{Settings: s}).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSyncConfigs_RotatesAndCyclesStacks(t *testing.T) {
	f := newFakeConfigPortainer(t)
	labels := map[string]string{"env": "dev"}
	f.addConfig("cfg-1", "app-config", "old", labels, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addStack(13, "frontend", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.addService("frontend_web", "frontend", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "DevOps")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	iStop12 := f.callIndex("POST /api/stacks/12/stop")
	iStop13 := f.callIndex("POST /api/stacks/13/stop")
	iDelete := f.callIndex("DELETE " + fakeDocker + "/configs/cfg-1")
	iCreate := f.callIndex("POST " + fakeDocker + "/configs/create")
	iStart12 := f.callIndex("POST /api/stacks/12/start")
	iStart13 := f.callIndex("POST /api/stacks/13/start")
	for name, idx := range map[string]int{"stop12": iStop12, "stop13": iStop13, "delete": iDelete, "create": iCreate, "start12": iStart12, "start13": iStart13} {
		if idx < 0 {
			t.Fatalf("expected call %s, calls: %v", name, f.calls)
		}
	}
	if !(iStop12 < iDelete && iStop13 < iDelete && iDelete < iCreate && iCreate < iStart12 && iCreate < iStart13) {
		t.Fatalf("wrong call order: %v", f.calls)
	}

	cfg := f.configByName("app-config")
	if cfg == nil || cfg.ID == "cfg-1" {
		t.Fatalf("config was not recreated: %+v", cfg)
	}
	if cfg.Data != base64.StdEncoding.EncodeToString([]byte("new")) {
		t.Fatalf("wrong config data: %s", cfg.Data)
	}
	if cfg.Labels["env"] != "dev" {
		t.Fatalf("labels not kept: %v", cfg.Labels)
	}
	if rc := f.rcUpdates[cfg.RCID]; !rc.Restricted || len(rc.Teams) != 1 || rc.Teams[0] != 3 {
		t.Fatalf("access not restricted to team 3: %+v", f.rcUpdates)
	}
}

func TestSyncConfigs_UnchangedOnlyUpdatesAccess(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "same", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "same")}}, "DevOps")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mutations := f.mutatingCalls(); len(mutations) != 1 || mutations[0] != "PUT /api/resource_controls/11" {
		t.Fatalf("expected only an access update, got %v", mutations)
	}
}

func TestSyncConfigs_PublicAccess(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "same", nil, 11)

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "same")}}, "")
	s.Public = true
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc := f.rcUpdates[11]; !rc.Public || rc.Restricted || rc.AdministratorsOnly {
		t.Fatalf("config not public: %+v", rc)
	}
}

func TestSyncConfigs_AdministratorsOnlyWithoutTeams(t *testing.T) {
	f := newFakeConfigPortainer(t)
	s := configSettings(f.server.URL, []ConfigEntry{{Name: "new-config", File: writeConfigFile(t, "fresh")}}, "")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := f.configByName("new-config")
	if cfg == nil {
		t.Fatal("config was not created")
	}
	if rc := f.rcUpdates[cfg.RCID]; !rc.AdministratorsOnly || rc.Teams == nil {
		t.Fatalf("config not administrators only: %+v", rc)
	}
}

func TestSyncConfigs_FailsBeforeChangesForServiceOutsideStack(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addService("rogue_service", "", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "is not part of a stack") {
		t.Fatalf("expected check error, got %v", err)
	}
	if mutations := f.mutatingCalls(); len(mutations) != 0 {
		t.Fatalf("expected no changes, got %v", mutations)
	}
}

func TestSyncConfigs_FailsBeforeChangesForStackOutsidePortainer(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addService("ghost_api", "ghost", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "not managed by Portainer") {
		t.Fatalf("expected check error, got %v", err)
	}
	if mutations := f.mutatingCalls(); len(mutations) != 0 {
		t.Fatalf("expected no changes, got %v", mutations)
	}
}

func TestSyncConfigs_CreateDoesNotTouchStacks(t *testing.T) {
	f := newFakeConfigPortainer(t)
	s := configSettings(f.server.URL, []ConfigEntry{{Name: "new-config", File: writeConfigFile(t, "fresh")}}, "QA")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := f.configByName("new-config")
	if cfg == nil {
		t.Fatal("config was not created")
	}
	if rc := f.rcUpdates[cfg.RCID]; len(rc.Teams) != 1 || rc.Teams[0] != 7 {
		t.Fatalf("access not applied: %+v", f.rcUpdates)
	}
	if f.callIndex("GET /api/stacks") != -1 {
		t.Fatalf("stacks should not be read: %v", f.calls)
	}
}

func TestSyncConfigs_RetriesDelete(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.deleteFailures["cfg-1"] = 2

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := f.countCalls("DELETE " + fakeDocker + "/configs/cfg-1"); n != 3 {
		t.Fatalf("expected 3 delete attempts, got %d", n)
	}
}

func TestSyncConfigs_StackSharedByConfigsCycledOnce(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-a", "old-a", nil, 11)
	f.addConfig("cfg-2", "app-b", "old-b", nil, 12)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-a"})
	f.addService("billing_worker", "billing", configRef{ID: "cfg-2", Name: "app-b"})

	s := configSettings(f.server.URL, []ConfigEntry{
		{Name: "app-a", File: writeConfigFile(t, "new-a")},
		{Name: "app-b", File: writeConfigFile(t, "new-b")},
	}, "")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := f.countCalls("POST /api/stacks/12/stop"); n != 1 {
		t.Fatalf("expected one stop, got %d", n)
	}
	if n := f.countCalls("POST /api/stacks/12/start"); n != 1 {
		t.Fatalf("expected one start, got %d", n)
	}
}

func TestSyncConfigs_InactiveStackIsNotStarted(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", 2)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.callIndex("POST /api/stacks/12/stop") != -1 || f.callIndex("POST /api/stacks/12/start") != -1 {
		t.Fatalf("inactive stack should not be stopped or started: %v", f.calls)
	}
}

func TestSyncConfigs_ToleratesStopStartConflicts(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.stopConflict = true
	f.startConflict = true

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("expected 409s to be tolerated, got %v", err)
	}
	if cfg := f.configByName("app-config"); cfg == nil || cfg.ID == "cfg-1" {
		t.Fatal("config was not rotated")
	}
}

func TestSyncConfigs_UnknownTeamFailsBeforeChanges(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "DevOps, Nope")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "unknown team(s): Nope") {
		t.Fatalf("expected unknown team error, got %v", err)
	}
	if mutations := f.mutatingCalls(); len(mutations) != 0 {
		t.Fatalf("expected no changes, got %v", mutations)
	}
}

func TestSyncConfigs_RestoresOldContentAndStartsStacks(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.createFailures["app-config"] = configCreateAttempts

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "old content restored") {
		t.Fatalf("expected restore error, got %v", err)
	}
	cfg := f.configByName("app-config")
	if cfg == nil || cfg.Data != base64.StdEncoding.EncodeToString([]byte("old")) {
		t.Fatalf("old content was not restored: %+v", cfg)
	}
	if f.callIndex("POST /api/stacks/12/start") == -1 {
		t.Fatalf("stopped stack was not started again: %v", f.calls)
	}
}

func TestSyncConfigs_MissingFileFailsBeforeRequests(t *testing.T) {
	f := newFakeConfigPortainer(t)
	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: filepath.Join(t.TempDir(), "missing")}}, "")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "failed to read config file") {
		t.Fatalf("expected file error, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("expected no requests, got %v", f.calls)
	}
}

func TestSyncConfigs_RunningCheckTimeout(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.tasksRunning = false

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	s.RunningCheck = true
	s.RunningTimeout = "30ms"
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "stack billing was not running") {
		t.Fatalf("expected running check timeout, got %v", err)
	}
}

func TestSyncConfigs_RunningCheckSuccess(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	s.RunningCheck = true
	s.RunningTimeout = "5s"
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.callIndex("GET "+fakeDocker+"/tasks") == -1 {
		t.Fatalf("running check did not read tasks: %v", f.calls)
	}
}

func TestGetConfig_ExactNameOnly(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "data", nil, 0)
	f.addConfig("cfg-2", "app-config-extra", "data2", nil, 0)
	c := newTestClient(t, f.server.Client(), f.server.URL, "primary")

	cfg, err := c.getConfig(context.Background(), 1, "app-config")
	if err != nil || cfg == nil || cfg.ID != "cfg-1" {
		t.Fatalf("expected cfg-1, got %+v, %v", cfg, err)
	}

	cfg, err = c.getConfig(context.Background(), 1, "app")
	if err != nil || cfg != nil {
		t.Fatalf("prefix-only match must not be returned, got %+v, %v", cfg, err)
	}
}

func TestWaitForStackRunning_FailsWithoutServices(t *testing.T) {
	f := newFakeConfigPortainer(t)
	c := newTestClient(t, f.server.Client(), f.server.URL, "primary")
	c.StackName = "empty"

	err := c.WaitForStackRunning(context.Background(), 1, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "has no services") {
		t.Fatalf("expected no services error, got %v", err)
	}
}

func TestSyncConfigs_DeleteFailsAtOnceOnPermanentError(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.deleteStatus = http.StatusForbidden

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	err := runConfigs(t, f, s)
	if err == nil || !strings.Contains(err.Error(), "failed to delete config") {
		t.Fatalf("expected delete error, got %v", err)
	}
	if n := f.countCalls("DELETE " + fakeDocker + "/configs/cfg-1"); n != 1 {
		t.Fatalf("expected 1 delete attempt, got %d", n)
	}
	if f.callIndex("POST /api/stacks/12/start") == -1 {
		t.Fatalf("stopped stack was not started again: %v", f.calls)
	}
}

func TestSyncConfigs_DeleteRetriesTransientErrors(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "old", nil, 11)
	f.addStack(12, "billing", stackStatusActive)
	f.addService("billing_api", "billing", configRef{ID: "cfg-1", Name: "app-config"})
	f.deleteStatus = http.StatusBadGateway

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "new")}}, "")
	if err := runConfigs(t, f, s); err == nil {
		t.Fatal("expected delete error")
	}
	if n := f.countCalls("DELETE " + fakeDocker + "/configs/cfg-1"); n != configDeleteAttempts {
		t.Fatalf("expected %d delete attempts, got %d", configDeleteAttempts, n)
	}
}

func TestSyncConfigs_CreatesAccessControlWhenMissing(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.addConfig("cfg-1", "app-config", "same", nil, 0)

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "app-config", File: writeConfigFile(t, "same")}}, "QA")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mutations := f.mutatingCalls(); len(mutations) != 1 || mutations[0] != "POST /api/resource_controls" {
		t.Fatalf("expected only an access control create, got %v", mutations)
	}
	cfg := f.configByName("app-config")
	if rc := f.rcUpdates[cfg.RCID]; cfg.RCID == 0 || rc.AdministratorsOnly || len(rc.Teams) != 1 || rc.Teams[0] != 7 {
		t.Fatalf("access control not created for team 7: %+v", f.rcUpdates)
	}
}

func TestSyncConfigs_AccessControlConflictUpdatesExisting(t *testing.T) {
	f := newFakeConfigPortainer(t)
	f.createNoRC = true

	s := configSettings(f.server.URL, []ConfigEntry{{Name: "new-config", File: writeConfigFile(t, "fresh")}}, "QA")
	if err := runConfigs(t, f, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := f.configByName("new-config")
	if cfg == nil {
		t.Fatal("config was not created")
	}
	iPost := f.callIndex("POST /api/resource_controls")
	iPut := f.callIndex("PUT /api/resource_controls/" + strconv.Itoa(cfg.RCID))
	if iPost < 0 || iPut < iPost {
		t.Fatalf("expected access control create, conflict, then update: %v", f.calls)
	}
	if rc := f.rcUpdates[cfg.RCID]; !rc.Restricted || len(rc.Teams) != 1 || rc.Teams[0] != 7 {
		t.Fatalf("existing access control not updated for team 7: %+v", f.rcUpdates)
	}
}

func TestExecute_ConfigsMode(t *testing.T) {
	f := newFakeConfigPortainer(t)
	file := writeConfigFile(t, "fresh")
	raw, _ := json.Marshal([]ConfigEntry{{Name: "new-config", File: file}})

	p := New("test")
	err := p.App.Run(context.Background(), []string{"plugin",
		"--server-url", f.server.URL,
		"--api-key", "test-key",
		"--server-environment", "primary",
		"--configs", string(raw),
		"--teams", "QA",
		"--running-check=false",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := f.configByName("new-config")
	if cfg == nil || cfg.Data != base64.StdEncoding.EncodeToString([]byte("fresh")) {
		t.Fatalf("config was not created: %+v", cfg)
	}
	if rc := f.rcUpdates[cfg.RCID]; len(rc.Teams) != 1 || rc.Teams[0] != 7 {
		t.Fatalf("access not applied: %+v", f.rcUpdates)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "/stacks") || strings.Contains(call, "/swarm") {
			t.Fatalf("configs mode must not touch stacks: %v", f.calls)
		}
	}
}
