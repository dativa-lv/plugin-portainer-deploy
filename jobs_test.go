// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSwarm simulates the Portainer API and Docker proxy for a stack with one job
// ("job-1", mystack_migrate) and one regular service ("svc-1", mystack_app).
type fakeSwarm struct {
	t *testing.T

	mu sync.Mutex
	// events records the order of service updates, e.g. "job-update", "job-complete", "force:svc-1".
	events []string
	// updateBody, updateQuery and updateAuth capture the job update request.
	updateBody  map[string]any
	updateQuery string
	updateAuth  string
	// iteration is the job iteration; it starts at 2 and is incremented by a job update.
	iteration uint64
	// taskState is the state of the tasks of the latest iteration after a job update.
	taskState string
	taskErr   string
	// polls counts task polls after the update; tasks complete on the second poll when taskState is "complete".
	polls int

	image          string
	registries     []portainerRegistry
	registryCalls  atomic.Int32
	forceJobCalled atomic.Bool
}

func newFakeSwarm(t *testing.T) *fakeSwarm {
	t.Helper()

	return &fakeSwarm{
		t:         t,
		iteration: 2,
		taskState: "complete",
		image:     "git.zzdats.lv/org/migrate:latest@sha256:0123456789abcdef",
	}
}

func (f *fakeSwarm) services() []portainerService {
	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_migrate"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{MaxConcurrent: 1, TotalCompletions: 1}
	job.JobStatus.JobIteration.Index = f.iteration
	restartDelay := int64(20 * time.Millisecond)
	job.Spec.TaskTemplate.RestartPolicy.Delay = &restartDelay

	app := portainerService{ID: "svc-1"}
	app.Spec.Name = "mystack_app"
	app.Spec.Mode.Replicated.Replicas = 1
	app.UpdateStatus.State = "completed"

	return []portainerService{job, app}
}

// rawJob is the raw inspect response of the job, including fields the plugin does not model.
func (f *fakeSwarm) rawJob() string {
	return `{
		"ID": "job-1",
		"Version": {"Index": 2194},
		"Spec": {
			"Name": "mystack_migrate",
			"Labels": {"com.docker.stack.namespace": "mystack"},
			"Mode": {"ReplicatedJob": {"MaxConcurrent": 1, "TotalCompletions": 1}},
			"TaskTemplate": {
				"ContainerSpec": {"Image": "` + f.image + `", "Args": ["migrate"]},
				"Resources": {"Limits": {"MemoryBytes": 9007199254740993}},
				"ForceUpdate": 3
			},
			"UpdateConfig": {"Parallelism": 1, "FailureAction": "pause"},
			"RollbackConfig": {"Parallelism": 1}
		},
		"JobStatus": {"JobIteration": {"Index": ` + jsonUint(f.iteration) + `}}
	}`
}

func jsonUint(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *fakeSwarm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/v1.47/services":
		_ = json.NewEncoder(w).Encode(f.services())
	case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/v1.47/services/job-1":
		w.Write([]byte(f.rawJob()))
	case r.Method == http.MethodPost && r.URL.Path == "/api/endpoints/1/docker/v1.47/services/job-1/update":
		body, _ := io.ReadAll(r.Body)
		f.updateBody = map[string]any{}
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.UseNumber()
		if err := dec.Decode(&f.updateBody); err != nil {
			f.t.Errorf("decode update body: %v", err)
		}
		f.updateQuery = r.URL.RawQuery
		f.updateAuth = r.Header.Get("X-Registry-Auth")
		f.iteration++
		f.events = append(f.events, "job-update")
		_, _ = w.Write([]byte(`{"Warnings":["image could not be accessed on a registry to record its digest"]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/v1.47/tasks" &&
		strings.Contains(r.URL.Query().Get("filters"), "com.docker.stack.namespace"):
		// Running check: all running tasks of the stack.
		_ = json.NewEncoder(w).Encode([]dockerTask{{ServiceID: "svc-1", Status: dockerTaskStatus{State: "running"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/v1.47/tasks":
		f.writeJobTasks(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/registries":
		f.registryCalls.Add(1)
		_ = json.NewEncoder(w).Encode(f.registries)
	case r.Method == http.MethodPut && r.URL.Path == "/api/endpoints/1/forceupdateservice":
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["serviceID"] == "job-1" {
			f.forceJobCalled.Store(true)
			http.Error(w, `{"message":"Error force update service","details":"Jobs may not have an update config"}`, http.StatusInternalServerError)
			return
		}
		f.events = append(f.events, "force:"+payload["serviceID"].(string))
		w.Write([]byte(`{}`))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (f *fakeSwarm) writeJobTasks(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Query().Get("filters"), `"job-1"`) {
		f.t.Errorf("expected tasks to be filtered by job service, got %s", r.URL.RawQuery)
	}

	// A completed task of the previous iteration must not count for the new one.
	tasks := []dockerTask{{
		ID: "old", ServiceID: "job-1", NodeID: "n1",
		JobIteration: swarmVersion{Index: 2}, Status: dockerTaskStatus{State: "complete"},
	}}

	if f.iteration > 2 {
		f.polls++
		state, errMsg := "running", ""
		if f.polls > 1 {
			state, errMsg = f.taskState, f.taskErr
		}
		if state == "complete" && !containsEvent(f.events, "job-complete") {
			f.events = append(f.events, "job-complete")
		}
		tasks = append(tasks, dockerTask{
			ID: "new", ServiceID: "job-1", NodeID: "n1",
			JobIteration: swarmVersion{Index: f.iteration}, Status: dockerTaskStatus{State: state, Err: errMsg},
		})
	}

	_ = json.NewEncoder(w).Encode(tasks)
}

func containsEvent(events []string, event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

func newJobTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	c.RunningCheck = true
	return c
}

// testJobTimeout is the job timeout used by tests that do not test timeouts.
const testJobTimeout = 500 * time.Millisecond

// --- UpdateStackServices with jobs ---

func TestUpdateStackServices_RerunsJobBeforeRegularServices(t *testing.T) {
	f := newFakeSwarm(t)
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if f.forceJobCalled.Load() {
		t.Fatal("job must not be updated through forceupdateservice")
	}

	want := []string{"job-update", "job-complete", "force:svc-1"}
	if strings.Join(f.events, ",") != strings.Join(want, ",") {
		t.Fatalf("expected events %v, got %v", want, f.events)
	}

	if f.updateQuery != "version=2194" {
		t.Fatalf("expected version=2194 query, got %q", f.updateQuery)
	}

	if _, ok := f.updateBody["UpdateConfig"]; ok {
		t.Fatal("update body must not contain UpdateConfig")
	}
	if _, ok := f.updateBody["RollbackConfig"]; ok {
		t.Fatal("update body must not contain RollbackConfig")
	}

	taskTemplate := f.updateBody["TaskTemplate"].(map[string]any)
	if taskTemplate["ForceUpdate"].(json.Number).String() != "4" {
		t.Fatalf("expected ForceUpdate 4, got %v", taskTemplate["ForceUpdate"])
	}

	containerSpec := taskTemplate["ContainerSpec"].(map[string]any)
	if containerSpec["Image"] != "git.zzdats.lv/org/migrate:latest" {
		t.Fatalf("expected image digest to be stripped, got %v", containerSpec["Image"])
	}
	if args, ok := containerSpec["Args"].([]any); !ok || len(args) != 1 || args[0] != "migrate" {
		t.Fatalf("expected unmodelled ContainerSpec fields to survive, got %v", containerSpec["Args"])
	}

	limits := taskTemplate["Resources"].(map[string]any)["Limits"].(map[string]any)
	if limits["MemoryBytes"].(json.Number).String() != "9007199254740993" {
		t.Fatalf("expected large integers to survive exactly, got %v", limits["MemoryBytes"])
	}

	labels, ok := f.updateBody["Labels"].(map[string]any)
	if !ok || labels["com.docker.stack.namespace"] != "mystack" {
		t.Fatalf("expected labels to survive, got %v", f.updateBody["Labels"])
	}
}

func TestUpdateStackServices_DoesNotWaitForJobsWhenNotAsked(t *testing.T) {
	f := newFakeSwarm(t)
	f.taskState = "running"
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, false, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(f.events, ","); got != "job-update,force:svc-1" {
		t.Fatalf("expected the job to be started without waiting, got %s", got)
	}
}

func TestUpdateStackServices_ServiceNameSelectsOnlyJob(t *testing.T) {
	f := newFakeSwarm(t)
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	c.ServiceName = "migrate"
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if containsEvent(f.events, "force:svc-1") {
		t.Fatalf("regular service must not be updated when only the job is selected: %v", f.events)
	}
	if !containsEvent(f.events, "job-update") {
		t.Fatalf("expected job update, got %v", f.events)
	}
}

func TestUpdateStackServices_JobFailureSkipsRegularServices(t *testing.T) {
	f := newFakeSwarm(t)
	f.taskState = "failed"
	f.taskErr = "task: non-zero exit (1)"
	srv := httptest.NewServer(f)
	defer srv.Close()

	// Swarm stops retrying long before the job timeout; the step must fail right away.
	c := newJobTestClient(t, srv)
	jobTimeout := 10 * time.Second
	started := time.Now()
	err := c.UpdateStackServices(context.Background(), 1, true, jobTimeout)
	if err == nil {
		t.Fatal("expected error for failing job")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("expected the failure to be detected before the job timeout, took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "Swarm is not retrying") {
		t.Fatalf("expected the job to be reported as given up, got: %v", err)
	}

	if !strings.Contains(err.Error(), "task: non-zero exit (1)") {
		t.Fatalf("expected task error in message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "mystack_migrate") {
		t.Fatalf("expected job name in message, got: %v", err)
	}
	if containsEvent(f.events, "force:svc-1") {
		t.Fatalf("regular service must not be updated after a failed job: %v", f.events)
	}
}

func TestUpdateStackServices_JobTimeoutListsTaskStates(t *testing.T) {
	f := newFakeSwarm(t)
	f.taskState = "running"
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	jobTimeout := 60 * time.Millisecond
	err := c.UpdateStackServices(context.Background(), 1, true, jobTimeout)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	if !strings.Contains(err.Error(), "0/1 tasks completed") || !strings.Contains(err.Error(), "tasks: running") {
		t.Fatalf("expected task states in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "60ms") {
		t.Fatalf("expected timeout in error, got: %v", err)
	}
}

func TestUpdateStackServices_JobUpdateErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := newFakeSwarm(t)
		switch {
		case r.URL.Path == "/api/endpoints/1/docker/v1.47/services":
			_ = json.NewEncoder(w).Encode(f.services())
		case r.URL.Path == "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(f.rawJob()))
		case r.URL.Path == "/api/registries":
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/update"):
			http.Error(w, `{"message":"update out of sequence"}`, http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout)
	if err == nil || !strings.Contains(err.Error(), "update out of sequence") {
		t.Fatalf("expected error with response body, got: %v", err)
	}
}

// --- Registry auth ---

func decodeRegistryAuth(t *testing.T, header string) map[string]any {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header %q is not standard base64: %v", header, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	return decoded
}

func TestRerunJob_SendsMatchingRegistryID(t *testing.T) {
	f := newFakeSwarm(t)
	f.registries = []portainerRegistry{
		{ID: 1, Name: "hub", URL: "docker.io"},
		{ID: 3, Name: "zzdats", URL: "git.zzdats.lv"},
		{ID: 4, Name: "other", URL: "git.zzdats.lv.example.com"},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	decoded := decodeRegistryAuth(t, f.updateAuth)
	if decoded["registryId"] != float64(3) {
		t.Fatalf("expected registryId 3, got %v", decoded)
	}
}

func TestRerunJob_NoRegistryHeaderWithoutMatch(t *testing.T) {
	f := newFakeSwarm(t)
	f.image = "alpine:3.22@sha256:0123456789abcdef"
	f.registries = []portainerRegistry{{ID: 3, Name: "zzdats", URL: "git.zzdats.lv"}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if f.updateAuth != "" {
		t.Fatalf("expected no X-Registry-Auth header, got %q", f.updateAuth)
	}
}

func TestRegistryIDForImage_FirstOfSameURLWins(t *testing.T) {
	registries := []portainerRegistry{
		{ID: 3, URL: "git.zzdats.lv"},
		{ID: 8, URL: "https://git.zzdats.lv"},
	}
	if got := registryIDForImage("git.zzdats.lv/org/app:1", registries); got != 3 {
		t.Fatalf("expected first matching registry 3, got %d", got)
	}
}

func TestNormalizeImageRepository(t *testing.T) {
	cases := map[string]string{
		"alpine":                                    "docker.io/library/alpine",
		"alpine:3.22@sha256:abc":                    "docker.io/library/alpine",
		"bitnami/redis:7":                           "docker.io/bitnami/redis",
		"git.zzdats.lv/org/app:tag":                 "git.zzdats.lv/org/app",
		"registry.local:5000/app":                   "registry.local:5000/app",
		"registry.local:5000/app:1.0@sha256:abc":    "registry.local:5000/app",
		"localhost/app:dev":                         "localhost/app",
		"index.docker.io/library/nginx:1":           "docker.io/library/nginx",
		"docker.io/alpine:3":                        "docker.io/library/alpine",
		"docker.io/bitnami/redis":                   "docker.io/bitnami/redis",
		"ghcr.io/dativa-lv/plugin-portainer-deploy": "ghcr.io/dativa-lv/plugin-portainer-deploy",
	}
	for image, want := range cases {
		if got := normalizeImageRepository(image); got != want {
			t.Errorf("normalizeImageRepository(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestRegistryIDForImage_LongestMatchAndURLForms(t *testing.T) {
	registries := []portainerRegistry{
		{ID: 1, URL: "https://git.zzdats.lv/"},
		{ID: 2, URL: "git.zzdats.lv/team"},
		{ID: 5, URL: "index.docker.io"},
	}

	if got := registryIDForImage("git.zzdats.lv/team/app:1", registries); got != 2 {
		t.Errorf("expected namespace registry 2, got %d", got)
	}
	if got := registryIDForImage("git.zzdats.lv/other/app:1", registries); got != 1 {
		t.Errorf("expected host registry 1, got %d", got)
	}
	if got := registryIDForImage("nginx:1", registries); got != 5 {
		t.Errorf("expected Docker Hub registry 5, got %d", got)
	}
	if got := registryIDForImage("quay.io/app", registries); got != 0 {
		t.Errorf("expected no registry, got %d", got)
	}
}

// --- jobCompletion ---

func TestJobCompletion_ReplicatedJobNeedsTotalCompletions(t *testing.T) {
	job := portainerService{}
	job.Spec.Name = "mystack_job"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{MaxConcurrent: 2, TotalCompletions: 3}

	tasks := []dockerTask{
		{Status: dockerTaskStatus{State: "complete"}},
		{Status: dockerTaskStatus{State: "complete"}},
		{Status: dockerTaskStatus{State: "failed", Err: "task: non-zero exit (2)"}},
	}
	err := jobCompletion(job, tasks)
	if err == nil || !strings.Contains(err.Error(), "2/3") || !strings.Contains(err.Error(), "non-zero exit (2)") {
		t.Fatalf("expected incomplete job with task error, got %v", err)
	}

	tasks = append(tasks, dockerTask{Status: dockerTaskStatus{State: "complete"}})
	if err := jobCompletion(job, tasks); err != nil {
		t.Fatalf("expected completed job, got %v", err)
	}
}

func TestJobCompletion_ReplicatedJobDefaultsToMaxConcurrent(t *testing.T) {
	job := portainerService{}
	job.Spec.Name = "mystack_job"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{MaxConcurrent: 3}

	tasks := []dockerTask{{Status: dockerTaskStatus{State: "complete"}}}
	if err := jobCompletion(job, tasks); err == nil || !strings.Contains(err.Error(), "1/3") {
		t.Fatalf("expected 1/3 completions, got %v", err)
	}
}

func TestJobCompletion_GlobalJobNeedsEveryNode(t *testing.T) {
	job := portainerService{}
	job.Spec.Name = "mystack_cleanup"
	job.Spec.Mode.GlobalJob = &struct{}{}

	if err := jobCompletion(job, nil); err == nil {
		t.Fatal("global job without tasks must not be complete")
	}

	tasks := []dockerTask{
		{NodeID: "n1", Status: dockerTaskStatus{State: "complete"}},
		{NodeID: "n2", Status: dockerTaskStatus{State: "failed", Err: "boom"}},
	}
	if err := jobCompletion(job, tasks); err == nil {
		t.Fatal("global job with an unfinished node must not be complete")
	}

	tasks = append(tasks, dockerTask{NodeID: "n2", Status: dockerTaskStatus{State: "complete"}})
	if err := jobCompletion(job, tasks); err != nil {
		t.Fatalf("expected completed global job, got %v", err)
	}
}

func TestWaitForJob_GlobalJobConfirmsNodesAndIgnoresInactiveNodes(t *testing.T) {
	var taskPolls, nodeCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":4}}}`))
		case "/api/endpoints/1/docker/v1.47/nodes":
			nodeCalls.Add(1)
			w.Write([]byte(`[
				{"ID":"n1","Spec":{"Availability":"active"},"Status":{"State":"ready"}},
				{"ID":"n2","Spec":{"Availability":"active"},"Status":{"State":"ready"}},
				{"ID":"n3","Spec":{"Availability":"drain"},"Status":{"State":"ready"}}
			]`))
		case "/api/endpoints/1/docker/v1.47/tasks":
			task := func(node, state string) dockerTask {
				return dockerTask{ServiceID: "job-1", NodeID: node, JobIteration: swarmVersion{Index: 4}, Status: dockerTaskStatus{State: state}}
			}
			// The drained node's task never completes and must not block the job.
			tasks := []dockerTask{task("n1", "complete"), task("n3", "assigned")}
			if taskPolls.Add(1) == 1 {
				tasks = append(tasks, task("n2", "running"))
			} else {
				tasks = append(tasks, task("n2", "complete"))
			}
			_ = json.NewEncoder(w).Encode(tasks)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_cleanup"
	job.Spec.Mode.GlobalJob = &struct{}{}

	c := newJobTestClient(t, srv)
	if _, err := c.waitForJob(context.Background(), 1, job, 4, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Poll 1: n2 still running; poll 2: n1 and n2 complete; poll 3: same nodes confirmed.
	if taskPolls.Load() != 3 {
		t.Fatalf("expected 3 task polls, got %d", taskPolls.Load())
	}
	// The node list is cached during the wait.
	if nodeCalls.Load() != 1 {
		t.Fatalf("expected 1 node lookup, got %d", nodeCalls.Load())
	}
}

func TestWaitForJob_GlobalJobConfirmedWithinShortTimeout(t *testing.T) {
	var taskPolls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":1}}}`))
		case "/api/endpoints/1/docker/v1.47/nodes":
			w.Write([]byte(`[{"ID":"n1","Spec":{"Availability":"active"},"Status":{"State":"ready"}}]`))
		case "/api/endpoints/1/docker/v1.47/tasks":
			taskPolls.Add(1)
			_ = json.NewEncoder(w).Encode([]dockerTask{{
				ServiceID: "job-1", NodeID: "n1", JobIteration: swarmVersion{Index: 1}, Status: dockerTaskStatus{State: "complete"},
			}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_cleanup"
	job.Spec.Mode.GlobalJob = &struct{}{}

	// The timeout is shorter than the poll interval (10ms), e.g. what is left of a shared deadline.
	c := newJobTestClient(t, srv)
	if _, err := c.waitForJob(context.Background(), 1, job, 1, time.Millisecond); err != nil {
		t.Fatalf("expected the completed global job to be confirmed, got %v", err)
	}
	if taskPolls.Load() != 2 {
		t.Fatalf("expected the tasks to be checked twice, got %d", taskPolls.Load())
	}
}

func TestWaitForJob_PermanentTaskListErrorFailsImmediately(t *testing.T) {
	var taskCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":1}}}`))
		case "/api/endpoints/1/docker/v1.47/nodes":
			http.Error(w, "access denied", http.StatusForbidden)
		case "/api/endpoints/1/docker/v1.47/tasks":
			taskCalls.Add(1)
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_cleanup"
	job.Spec.Mode.GlobalJob = &struct{}{}

	c := newJobTestClient(t, srv)
	timedOut, err := c.waitForJob(context.Background(), 1, job, 1, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "403") || timedOut {
		t.Fatalf("expected an immediate 403 error, got %v", err)
	}
	if taskCalls.Load() != 1 {
		t.Fatalf("expected no retries, got %d task calls", taskCalls.Load())
	}
}

func TestWaitForJob_CancellationIsNotReportedAsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":1}}}`))
		case "/api/endpoints/1/docker/v1.47/tasks":
			w.Write([]byte(`[{"ID":"t1","JobIteration":{"Index":1},"Status":{"State":"running"}}]`))
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_migrate"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{TotalCompletions: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := newJobTestClient(t, srv)
	timedOut, err := c.waitForJob(ctx, 1, job, 1, 10*time.Second)
	if err == nil || timedOut {
		t.Fatalf("expected a cancellation error, got %v", err)
	}
}

func TestWaitForStackJobs_RetriesTransientListErrors(t *testing.T) {
	var listCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if listCalls.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(newFakeSwarm(t).services())
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.WaitForStackJobs(context.Background(), 1, map[string]uint64{"job-1": 2}, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listCalls.Load() != 2 {
		t.Fatalf("expected one retry, got %d calls", listCalls.Load())
	}
}

func TestStackFileDeclaresJobs(t *testing.T) {
	cases := map[string]bool{
		"services:\n  app:\n    image: app\n":                              false,
		"services:\n  migrate:\n    deploy:\n      mode: replicated-job\n": true,
		"services:\n  cleanup:\n    deploy:\n      mode: \"global-job\"\n": true,
		"services:\n  app:\n    deploy:\n      mode: ${MODE}\n":            true,
		"services:\n  app:\n    image: app:${TAG}\n":                       false,
	}
	for content, want := range cases {
		c := &Client{StackPath: writeTempStackFile(t, content)}
		if got := c.StackFileDeclaresJobs(); got != want {
			t.Errorf("StackFileDeclaresJobs(%q) = %v, want %v", content, got, want)
		}
	}

	if !(&Client{StackPath: "/nonexistent/stack.yml"}).StackFileDeclaresJobs() {
		t.Error("an unreadable stack file must count as declaring jobs")
	}
}

func TestPollUntil_LastAttemptAtDeadline(t *testing.T) {
	var calls atomic.Int32
	// The timeout is shorter than the interval: backoff.Retry would give up after the first attempt.
	timedOut, err := pollUntil(context.Background(), 30*time.Millisecond, time.Second, func() error {
		if calls.Add(1) == 1 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || timedOut {
		t.Fatalf("expected success on the attempt at the deadline, got timedOut=%v err=%v", timedOut, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}

	timedOut, err = pollUntil(context.Background(), 20*time.Millisecond, 5*time.Millisecond, func() error {
		return errors.New("still running")
	})
	if !timedOut || err == nil || err.Error() != "still running" {
		t.Fatalf("expected a timeout with the last error, got timedOut=%v err=%v", timedOut, err)
	}
}

func TestWaitForJob_LooksUpIterationOnlyUntilFound(t *testing.T) {
	var inspects, taskPolls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			inspects.Add(1)
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":3}}}`))
		case "/api/endpoints/1/docker/v1.47/tasks":
			state := "running"
			if taskPolls.Add(1) > 3 {
				state = "complete"
			}
			_ = json.NewEncoder(w).Encode([]dockerTask{{ID: "t1", JobIteration: swarmVersion{Index: 3}, Status: dockerTaskStatus{State: state}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_migrate"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{TotalCompletions: 1}

	c := newJobTestClient(t, srv)
	if _, err := c.waitForJob(context.Background(), 1, job, 3, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inspects.Load() != 1 || taskPolls.Load() != 4 {
		t.Fatalf("expected 1 inspect and 4 task polls, got %d and %d", inspects.Load(), taskPolls.Load())
	}
}

func TestWaitForStackRunning_LastAttemptAtDeadline(t *testing.T) {
	var taskCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/services") {
			_ = json.NewEncoder(w).Encode([]portainerService{replicatedService("svc-1", "mystack_app", 1, "completed")})
			return
		}
		state := "starting"
		if taskCalls.Add(1) > 1 {
			state = "running"
		}
		_ = json.NewEncoder(w).Encode([]dockerTask{{ServiceID: "svc-1", Status: dockerTaskStatus{State: state}}})
	}))
	defer srv.Close()

	// The timeout is shorter than the poll interval, so only the attempt at the deadline can pass.
	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	c.pollInterval = time.Second
	if err := c.WaitForStackRunning(context.Background(), 1, 50*time.Millisecond); err != nil {
		t.Fatalf("expected the attempt at the deadline to pass, got %v", err)
	}
}

func TestCheckHealth_TimeoutNamesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	err := c.CheckHealth(context.Background(), srv.URL, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not healthy within 30ms") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected a timeout with the last status, got %v", err)
	}
}

func TestUpdateStackServices_RetriesTransientListErrors(t *testing.T) {
	f := newFakeSwarm(t)
	var listCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/endpoints/1/docker/v1.47/services" && listCalls.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(f.events, ","); got != "job-update,job-complete,force:svc-1" {
		t.Fatalf("unexpected events %s", got)
	}
}

func TestStackJobIterations_RetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(newFakeSwarm(t).services())
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	iterations, err := c.StackJobIterations(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(iterations) != 1 || iterations["job-1"] != 2 {
		t.Fatalf("expected only the job's iteration, got %v", iterations)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one retry, got %d calls", calls.Load())
	}
}

func TestStackJobIterations_DoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if _, err := c.StackJobIterations(context.Background(), 1); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no retries, got %d calls", calls.Load())
	}
}

func TestWaitForJob_GlobalJobWithoutTasksDoesNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(`{"ID":"job-1","JobStatus":{"JobIteration":{"Index":1}}}`))
		case "/api/endpoints/1/docker/v1.47/nodes":
			w.Write([]byte(`[{"ID":"n1","Spec":{"Availability":"active"},"Status":{"State":"ready"}}]`))
		case "/api/endpoints/1/docker/v1.47/tasks":
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_cleanup"
	job.Spec.Mode.GlobalJob = &struct{}{}

	// Swarm may not have created the tasks yet, so a job without tasks must not pass.
	c := newJobTestClient(t, srv)
	_, err := c.waitForJob(context.Background(), 1, job, 1, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no tasks on any ready, active node") {
		t.Fatalf("expected a timeout naming the missing tasks, got %v", err)
	}
}

func TestRerunJob_RetriesUpdateOutOfSequence(t *testing.T) {
	f := newFakeSwarm(t)
	var updates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/update") && updates.Add(1) == 1 {
			http.Error(w, `{"message":"rpc error: code = Unknown desc = update out of sequence"}`, http.StatusInternalServerError)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updates.Load() != 2 {
		t.Fatalf("expected the update to be retried once, got %d updates", updates.Load())
	}
	if got := strings.Join(f.events, ","); got != "job-update,job-complete,force:svc-1" {
		t.Fatalf("unexpected events %s", got)
	}
}

func TestPrepareJobRerunSpec_KeepsDigestOnlyImage(t *testing.T) {
	cases := map[string]string{
		"registry.example.com/app@sha256:abc":       "registry.example.com/app@sha256:abc",
		"registry.example.com:5000/app@sha256:abc":  "registry.example.com:5000/app@sha256:abc",
		"registry.example.com/app:1.2@sha256:abc":   "registry.example.com/app:1.2",
		"registry.example.com:5000/app:1@sha256:ab": "registry.example.com:5000/app:1",
		"alpine:3.22": "alpine:3.22",
	}
	for image, want := range cases {
		spec := map[string]any{"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": image}}}
		got, err := prepareJobRerunSpec(spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		written := spec["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)["Image"]
		if got != want || written != want {
			t.Errorf("image %q: got %q, spec %q, want %q", image, got, written, want)
		}
	}
}

func TestPrepareJobRerunSpec_UsesStackImageLabel(t *testing.T) {
	// The spec image is pinned by the stack deploy; the label holds the image as written in the stack file.
	cases := map[string]string{
		"app:1.2":                   "app:1.2",
		"app:1.2@sha256:pinnedbyme": "app:1.2@sha256:pinnedbyme",
		"app@sha256:pinnedbyme":     "app@sha256:pinnedbyme",
	}
	for label, want := range cases {
		spec := map[string]any{
			"Labels":       map[string]any{"com.docker.stack.image": label},
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "app:1.2@sha256:resolvedatdeploy"}},
		}
		got, err := prepareJobRerunSpec(spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		written := spec["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)["Image"]
		if got != want || written != want {
			t.Errorf("label %q: got %q, spec %q, want %q", label, got, written, want)
		}
	}
}

// --- WaitForStackRunning / WaitForStackJobs ---

func TestWaitForStackRunning_IgnoresJobServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/services") {
			job := portainerService{ID: "job-1"}
			job.Spec.Name = "mystack_migrate"
			job.Spec.Mode.ReplicatedJob = &replicatedJobMode{MaxConcurrent: 1, TotalCompletions: 1}
			// Jobs never have running tasks; a stale update status must not block either.
			job.UpdateStatus.State = "paused"
			app := portainerService{ID: "svc-1"}
			app.Spec.Name = "mystack_app"
			app.Spec.Mode.Replicated.Replicas = 1
			_ = json.NewEncoder(w).Encode([]portainerService{job, app})
			return
		}
		_ = json.NewEncoder(w).Encode([]dockerTask{{ServiceID: "svc-1", Status: dockerTaskStatus{State: "running"}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	c.StackName = "mystack"
	if err := c.WaitForStackRunning(context.Background(), 1, 50*time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStackJobs_WaitsForLatestIteration(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := newFakeSwarm(t)
		switch r.URL.Path {
		case "/api/endpoints/1/docker/v1.47/services":
			_ = json.NewEncoder(w).Encode(f.services())
		case "/api/endpoints/1/docker/v1.47/services/job-1":
			w.Write([]byte(f.rawJob()))
		case "/api/endpoints/1/docker/v1.47/tasks":
			state := "running"
			if polls.Add(1) > 1 {
				state = "complete"
			}
			_ = json.NewEncoder(w).Encode([]dockerTask{{
				ServiceID: "job-1", JobIteration: swarmVersion{Index: 2}, Status: dockerTaskStatus{State: state},
			}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.WaitForStackJobs(context.Background(), 1, map[string]uint64{"job-1": 1}, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if polls.Load() < 2 {
		t.Fatalf("expected to poll until the job completed, got %d polls", polls.Load())
	}
}

func TestWaitForStackJobs_SkipsJobsNotChangedByDeploy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/endpoints/1/docker/v1.47/services" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(newFakeSwarm(t).services())
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	// The job's iteration (2) is the same as before the deploy, e.g. a job that failed long ago.
	if err := c.WaitForStackJobs(context.Background(), 1, map[string]uint64{"job-1": 2}, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStackJobs_SharesTimeoutAcrossJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/endpoints/1/docker/v1.47/services":
			jobs := make([]portainerService, 0, 3)
			for _, id := range []string{"job-a", "job-b", "job-c"} {
				job := portainerService{ID: id}
				job.Spec.Name = "mystack_" + id
				job.Spec.Mode.ReplicatedJob = &replicatedJobMode{MaxConcurrent: 1, TotalCompletions: 1}
				jobs = append(jobs, job)
			}
			_ = json.NewEncoder(w).Encode(jobs)
		case strings.HasPrefix(r.URL.Path, "/api/endpoints/1/docker/v1.47/services/"):
			w.Write([]byte(`{"JobStatus":{"JobIteration":{}}}`))
		case r.URL.Path == "/api/endpoints/1/docker/v1.47/tasks":
			w.Write([]byte(`[{"Status":{"State":"running"}}]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newJobTestClient(t, srv)
	jobTimeout := 150 * time.Millisecond
	started := time.Now()
	err := c.WaitForStackJobs(context.Background(), 1, map[string]uint64{}, jobTimeout)
	if err == nil || !strings.Contains(err.Error(), "within 150ms (job-timeout") {
		t.Fatalf("expected a timeout naming the shared job-timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("expected the jobs to share one 150ms timeout, took %s", elapsed)
	}
}

func TestValidate_JobTimeoutFallsBackToRunningCheckTimeout(t *testing.T) {
	newPlugin := func() *Plugin {
		return &Plugin{Settings: &Settings{
			APIKey:            "key",
			ServerURL:         "https://portainer.example",
			ServerEnvironment: "prod",
			StackName:         "mystack",
			RunningCheck:      true,
			RunningTimeout:    "2m",
		}}
	}

	p := newPlugin()
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Settings.jobTimeout != 2*time.Minute || p.Settings.deployTimeout != defaultDeployTimeout {
		t.Fatalf("expected job timeout 2m and default deploy timeout, got %s and %s", p.Settings.jobTimeout, p.Settings.deployTimeout)
	}

	p = newPlugin()
	p.Settings.JobTimeout = "10m"
	p.Settings.DeployTimeout = "5m"
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Settings.jobTimeout != 10*time.Minute || p.Settings.deployTimeout != 5*time.Minute {
		t.Fatalf("expected job timeout 10m and deploy timeout 5m, got %s and %s", p.Settings.jobTimeout, p.Settings.deployTimeout)
	}
}

func TestWaitForJob_RemovedServiceFailsImmediately(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"service job-1 not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	job := portainerService{ID: "job-1"}
	job.Spec.Name = "mystack_migrate"
	job.Spec.Mode.ReplicatedJob = &replicatedJobMode{TotalCompletions: 1}

	c := newJobTestClient(t, srv)
	timedOut, err := c.waitForJob(context.Background(), 1, job, 1, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "404") || timedOut {
		t.Fatalf("expected an immediate 404 error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no retries, got %d calls", calls.Load())
	}
}

func TestRerunJob_NoRegistryHeaderWithoutImage(t *testing.T) {
	f := newFakeSwarm(t)
	f.image = ""
	f.registries = []portainerRegistry{{ID: 1, Name: "hub", URL: "docker.io"}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newJobTestClient(t, srv)
	if err := c.UpdateStackServices(context.Background(), 1, true, testJobTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.updateAuth != "" {
		t.Fatalf("expected no X-Registry-Auth header without an image, got %q", f.updateAuth)
	}
}

// --- WaitForStackDeployment ---

func stackStatusServer(t *testing.T, responses []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stacks/10" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		i := int(calls.Add(1)) - 1
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Write([]byte(responses[i]))
	}))
	return srv, &calls
}

func TestWaitForStackDeployment_DeployingThenActive(t *testing.T) {
	srv, calls := stackStatusServer(t, []string{
		`{"Id":10,"Status":3,"DeploymentStatus":[{"Status":3,"Time":1}]}`,
		`{"Id":10,"Status":3,"DeploymentStatus":[{"Status":3,"Time":1}]}`,
		`{"Id":10,"Status":1,"DeploymentStatus":[{"Status":3,"Time":1},{"Status":1,"Time":2}]}`,
	})
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.WaitForStackDeployment(context.Background(), 10, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 polls, got %d", calls.Load())
	}
}

func TestWaitForStackDeployment_DeployingThenError(t *testing.T) {
	srv, calls := stackStatusServer(t, []string{
		`{"Id":10,"Status":3,"DeploymentStatus":[{"Status":3,"Time":1}]}`,
		`{"Id":10,"Status":4,"DeploymentStatus":[{"Status":3,"Time":1},{"Status":4,"Time":2,"Message":"network \"missingnet\" is declared as external, but could not be found"}]}`,
	})
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	err := c.WaitForStackDeployment(context.Background(), 10, time.Second)
	if err == nil {
		t.Fatal("expected deployment error")
	}
	if !strings.Contains(err.Error(), `network "missingnet" is declared as external`) {
		t.Fatalf("expected Portainer message in error, got: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected polling to stop at the error, got %d polls", calls.Load())
	}
}

func TestWaitForStackDeployment_ClientErrorIsPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Unable to find a stack"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	err := c.WaitForStackDeployment(context.Background(), 10, time.Second)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no retries on a client error, got %d calls", calls.Load())
	}
}

func TestWaitForStackDeployment_RetriesServerErrorsAndRateLimits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "restarting", http.StatusBadGateway)
			return
		case 2:
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"Id":10,"Status":1,"DeploymentStatus":[{"Status":1,"Time":2}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.WaitForStackDeployment(context.Background(), 10, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestWaitForStackDeployment_TimeoutNamesSetting(t *testing.T) {
	srv, _ := stackStatusServer(t, []string{`{"Id":10,"Status":3,"DeploymentStatus":[{"Status":3,"Time":1}]}`})
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	err := c.WaitForStackDeployment(context.Background(), 10, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deploy-timeout") || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("expected a timeout naming deploy-timeout, got %v", err)
	}
}

func TestWaitForStackDeployment_InvalidResponseIsPermanent(t *testing.T) {
	srv, calls := stackStatusServer(t, []string{`<html>not json</html>`})
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.WaitForStackDeployment(context.Background(), 10, time.Second); err == nil {
		t.Fatal("expected error for an invalid response")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no retries, got %d calls", calls.Load())
	}
}

func TestWaitForStackDeployment_SkipsWithoutStatusFields(t *testing.T) {
	srv, calls := stackStatusServer(t, []string{`{"Id":10,"Name":"test-stack","ResourceControl":{"Id":99}}`})
	defer srv.Close()

	c := newTestClient(t, srv.Client(), srv.URL, "prod")
	if err := c.WaitForStackDeployment(context.Background(), 10, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected a single poll, got %d", calls.Load())
	}
}

// --- Validate ---

func TestValidate_JobSettings(t *testing.T) {
	base := func() *Plugin {
		return &Plugin{Settings: &Settings{
			APIKey:            "key",
			ServerURL:         "https://portainer.example",
			ServerEnvironment: "prod",
			StackName:         "mystack",
			RunningCheck:      true,
			RunningTimeout:    "1m",
		}}
	}

	p := base()
	p.Settings.JobTimeout = "5m"
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	// Zero means no limit for every timeout; the job timeout follows the running-check timeout.
	p = base()
	p.Settings.RunningTimeout = "0s"
	p.Settings.HealthCheck = true
	p.Settings.HealthCheckURL = "https://example.com/health"
	p.Settings.HealthCheckTimeout = "0s"
	p.Settings.DeployTimeout = "0s"
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected validation error for zero timeouts: %v", err)
	}
	if p.Settings.jobTimeout != 0 || p.Settings.deployTimeout != 0 {
		t.Fatalf("expected no job and deploy limits, got %s and %s", p.Settings.jobTimeout, p.Settings.deployTimeout)
	}

	// Negative running-check and health-check timeouts are mistakes.
	p = base()
	p.Settings.RunningTimeout = "-1m"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for negative running-check-timeout")
	}

	p = base()
	p.Settings.HealthCheck = true
	p.Settings.HealthCheckURL = "https://example.com/health"
	p.Settings.HealthCheckTimeout = "-1m"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for negative health-check-timeout")
	}

	p = base()
	p.Settings.JobTimeout = "five minutes"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for invalid job-timeout")
	}

	// A malformed job-timeout is rejected even without the running check, where it is not used.
	p = base()
	p.Settings.RunningCheck = false
	p.Settings.JobTimeout = "five minutes"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for invalid job-timeout without the running check")
	}

	p = base()
	p.Settings.JobTimeout = "-5m"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for negative job-timeout")
	}

	p = base()
	p.Settings.DeployTimeout = "-5m"
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for negative deploy-timeout")
	}
}
