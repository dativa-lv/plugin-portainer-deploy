// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rs/zerolog/log"
)

// portainerRegistry is the API response shape for /api/registries list entries.
type portainerRegistry struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
	URL  string `json:"URL"`
}

// rawService is a Swarm service whose spec is kept as a generic map, so that fields the
// plugin does not model survive the inspect/update round trip.
type rawService struct {
	ID        string           `json:"ID"`
	Version   swarmVersion     `json:"Version"`
	Spec      map[string]any   `json:"Spec"`
	JobStatus serviceJobStatus `json:"JobStatus"`
}

// rerunJobService starts a new iteration of a job service and returns the job iteration
// that was current before the update.
//
// Portainer's forceupdateservice endpoint cannot be used for jobs: it inspects the service
// with defaults inserted, which adds an UpdateConfig, and Swarm rejects any job spec that has one.
// The service is updated through Portainer's Docker proxy instead.
func (c *Client) rerunJobService(ctx context.Context, endpointID int, serviceID string, registries []portainerRegistry) (uint64, error) {
	const attempts = 3

	for attempt := 1; ; attempt++ {
		previousIteration, err := c.updateJobService(ctx, endpointID, serviceID, registries)

		// Swarm rejects an update based on an outdated service version, e.g. when the service
		// changed between the inspect and the update; the update is then repeated from a fresh inspect.
		status, ok := errors.AsType[*statusError](err)
		if ok && attempt < attempts && strings.Contains(status.Body, "update out of sequence") {
			log.Warn().Msgf("Service %s changed during the update; retrying", serviceID)

			continue
		}

		return previousIteration, err
	}
}

func (c *Client) updateJobService(ctx context.Context, endpointID int, serviceID string, registries []portainerRegistry) (uint64, error) {
	service, err := c.inspectRawService(ctx, endpointID, serviceID)
	if err != nil {
		return 0, err
	}

	image, err := prepareJobRerunSpec(service.Spec)
	if err != nil {
		return 0, err
	}

	header := http.Header{}

	if image != "" {
		if registryID := registryIDForImage(image, registries); registryID != 0 {
			value, err := registryAuthHeaderValue(registryID)
			if err != nil {
				return 0, err
			}

			header.Set("X-Registry-Auth", value)
		}
	}

	var result struct {
		Warnings []string `json:"Warnings"`
	}

	updateURL := fmt.Sprintf("%s/update?version=%d", c.serviceURL(endpointID, serviceID), service.Version.Index)

	err = c.sendJSON(ctx, http.MethodPost, updateURL, service.Spec, header, &result)
	if err != nil {
		return 0, fmt.Errorf("failed to update job service: %w", err)
	}

	for _, warning := range result.Warnings {
		log.Warn().Msgf("Service %s: %s", serviceID, warning)
	}

	return service.JobStatus.JobIteration.Index, nil
}

func (c *Client) inspectRawService(ctx context.Context, endpointID int, serviceID string) (*rawService, error) {
	var service rawService

	err := c.getJSON(ctx, c.serviceURL(endpointID, serviceID), &service)
	if err != nil {
		return nil, err
	}

	if service.Spec == nil {
		return nil, fmt.Errorf("service %s has no spec", serviceID)
	}

	return &service, nil
}

func (c *Client) serviceURL(endpointID int, serviceID string) string {
	return c.dockerURL(endpointID, "/services/"+url.PathEscape(serviceID))
}

// dockerURL returns the URL of a Docker API path behind Portainer's Docker proxy, on the API version
// the plugin is written against.
func (c *Client) dockerURL(endpointID int, path string) string {
	return fmt.Sprintf("%s/api/endpoints/%d/docker/v1.47%s", c.ServerURL, endpointID, path)
}

// prepareJobRerunSpec changes a job spec in place so that Swarm starts a new job iteration
// with a freshly pulled image. It returns the image reference that will be pulled.
func prepareJobRerunSpec(spec map[string]any) (string, error) {
	taskTemplate, ok := spec["TaskTemplate"].(map[string]any)
	if !ok {
		return "", errors.New("service spec has no TaskTemplate")
	}

	forceUpdate := uint64(0)

	if current, ok := taskTemplate["ForceUpdate"].(json.Number); ok {
		value, err := strconv.ParseUint(current.String(), 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid ForceUpdate value %q: %w", current, err)
		}

		forceUpdate = value
	}

	taskTemplate["ForceUpdate"] = forceUpdate + 1

	image := ""

	if containerSpec, ok := taskTemplate["ContainerSpec"].(map[string]any); ok {
		image, _ = containerSpec["Image"].(string)

		// Stack deploys pin the digest the tag pointed to at deploy time and keep the image as written in
		// the stack file in a label. Deploying that image again pulls what the tag points to now, while a
		// digest pinned in the stack file is kept.
		labels, _ := spec["Labels"].(map[string]any)

		if stackImage, _ := labels["com.docker.stack.image"].(string); stackImage != "" {
			image = stackImage
		} else if name, _, found := strings.Cut(image, "@"); found && hasImageTag(name) {
			// Without the label, drop the digest unless the image is pinned only by digest.
			image = name
		}

		if image != "" {
			containerSpec["Image"] = image
		}
	}

	// Swarm rejects job specs with an update or rollback config.
	delete(spec, "UpdateConfig")
	delete(spec, "RollbackConfig")

	return image, nil
}

// registryIDForImage returns the Portainer registry whose credentials Portainer should use to pull
// the image, or 0 when no registry matches. The registry with the longest URL matching the image
// repository wins; of several registries with the same URL, the first one listed is used.
func registryIDForImage(image string, registries []portainerRegistry) int {
	repository := normalizeImageRepository(image)
	bestID, bestLen, ties := 0, 0, 0

	for _, registry := range registries {
		registryURL := normalizeRegistryURL(registry.URL)
		if registryURL == "" || (repository != registryURL && !strings.HasPrefix(repository, registryURL+"/")) {
			continue
		}

		switch {
		case len(registryURL) > bestLen:
			bestID, bestLen, ties = registry.ID, len(registryURL), 0
		case len(registryURL) == bestLen:
			ties++
		}
	}

	switch {
	case bestID == 0:
		log.Debug().Msgf("No Portainer registry matches image %q; pulling without registry credentials", image)
	case ties > 0:
		log.Warn().Msgf("%d Portainer registries match image %q equally; using registry %d", ties+1, image, bestID)
	default:
		log.Debug().Msgf("Using Portainer registry %d for image %q", bestID, image)
	}

	return bestID
}

// hasImageTag reports whether an image reference without digest has a tag.
// A colon after the last slash separates the tag; a colon before it belongs to a registry port.
func hasImageTag(name string) bool {
	return strings.LastIndex(name, ":") > strings.LastIndex(name, "/")
}

// normalizeImageRepository returns the image repository including its registry host,
// without tag or digest (e.g. "alpine:3" becomes "docker.io/library/alpine").
func normalizeImageRepository(image string) string {
	repository, _, _ := strings.Cut(image, "@")

	if hasImageTag(repository) {
		repository = repository[:strings.LastIndex(repository, ":")]
	}

	host, rest, found := strings.Cut(repository, "/")
	if !found || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		if !found {
			repository = "library/" + repository
		}

		return "docker.io/" + repository
	}

	if host == "docker.io" || host == "index.docker.io" || host == "registry-1.docker.io" {
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}

		return "docker.io/" + rest
	}

	return repository
}

// normalizeRegistryURL turns a Portainer registry URL into the form used in image references.
func normalizeRegistryURL(registryURL string) string {
	registryURL = strings.TrimSpace(registryURL)
	registryURL = strings.TrimPrefix(registryURL, "https://")
	registryURL = strings.TrimPrefix(registryURL, "http://")
	registryURL = strings.TrimSuffix(registryURL, "/")

	switch registryURL {
	case "index.docker.io", "registry-1.docker.io", "index.docker.io/v1":
		return "docker.io"
	}

	return registryURL
}

// registryAuthHeaderValue builds the X-Registry-Auth value that makes Portainer's Docker proxy
// substitute the stored credentials of the given registry. Portainer decodes it as standard base64.
func registryAuthHeaderValue(registryID int) (string, error) {
	data, err := json.Marshal(map[string]int{"registryId": registryID})
	if err != nil {
		return "", fmt.Errorf("failed to marshal registry auth header: %w", err)
	}

	return base64.StdEncoding.EncodeToString(data), nil
}

func (c *Client) getRegistries(ctx context.Context) ([]portainerRegistry, error) {
	var registries []portainerRegistry

	err := c.getJSON(ctx, c.ServerURL+"/api/registries", &registries)
	if err != nil {
		return nil, err
	}

	return registries, nil
}

// StackJobIterations returns the current job iteration of every job service in the stack, by service ID.
func (c *Client) StackJobIterations(ctx context.Context, endpointID int) (map[string]uint64, error) {
	services, err := c.listStackServices(ctx, endpointID)
	if err != nil {
		return nil, err
	}

	iterations := map[string]uint64{}

	for _, service := range services {
		if service.isJob() {
			iterations[service.ID] = service.JobStatus.JobIteration.Index
		}
	}

	return iterations, nil
}

// listStackServices lists the stack's services, retrying transient errors a few times.
func (c *Client) listStackServices(ctx context.Context, endpointID int) ([]portainerService, error) {
	services, err := backoff.Retry(ctx, func() ([]portainerService, error) {
		services, err := c.getStackServices(ctx, endpointID)
		if err != nil && permanentError(err) {
			return nil, backoff.Permanent(err)
		}

		return services, err
	}, backoff.WithMaxTries(3), backoff.WithBackOff(backoff.NewConstantBackOff(c.pollInterval)))
	if err != nil {
		return nil, fmt.Errorf("failed to list services for stack %s: %w", c.StackName, err)
	}

	return services, nil
}

// StackFileDeclaresJobs reports whether the stack file may declare job services. It errs on the side
// of true: an unreadable file and a service mode set through a variable count as declaring jobs.
func (c *Client) StackFileDeclaresJobs() bool {
	content, err := c.readYAMLAsString()
	if err != nil {
		return true
	}

	return strings.Contains(content, "-job") || modeFromVariable.MatchString(content)
}

// modeFromVariable matches a service mode set through a variable, e.g. `mode: ${MODE}`.
var modeFromVariable = regexp.MustCompile(`(?m)^\s*mode:\s*["']?\$`)

// WaitForStackJobs waits for the jobs a stack deploy started to complete: jobs that are new or whose
// job iteration changed compared to before, the iterations returned by StackJobIterations before the
// deploy. Swarm runs these jobs at the same time, so the job timeout applies to all of them together.
func (c *Client) WaitForStackJobs(ctx context.Context, endpointID int, before map[string]uint64, timeout time.Duration) error {
	services, err := c.listStackServices(ctx, endpointID)
	if err != nil {
		return err
	}

	// Zero means no limit.
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for _, service := range services {
		if !service.isJob() {
			continue
		}

		// Swarm increments the job iteration in the same store update that changes the service spec,
		// so the deploy's iteration is visible as soon as Portainer reports the deployment finished.
		iteration := service.JobStatus.JobIteration.Index

		if previous, existed := before[service.ID]; existed && iteration == previous {
			log.Info().Msgf("Job %s was not changed by this deploy; not waiting for it", service.Spec.Name)

			continue
		}

		logGroup("Wait for job " + service.Spec.Name)
		log.Info().Msgf("Waiting for job %s to complete...", service.Spec.Name)

		// A passed deadline still allows one check, since the job may have completed meanwhile.
		remaining := time.Duration(0)
		if !deadline.IsZero() {
			remaining = max(time.Until(deadline), time.Millisecond)
		}

		timedOut, err := c.waitForJob(ctx, endpointID, service, iteration, remaining)
		if timedOut {
			return fmt.Errorf("jobs did not complete within %s (job-timeout, shared by the jobs of a deploy): %w", timeout, err)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// waitForJob polls a job service until its current iteration is at least minIteration and that
// iteration has completed. Failed tasks do not end the wait while Swarm retries them according to
// the service's restart policy; the job fails when Swarm stops retrying or the timeout elapses.
// timedOut tells the caller, which knows which timeout applied, that the timeout elapsed.
func (c *Client) waitForJob(
	ctx context.Context, endpointID int, job portainerService, minIteration uint64, timeout time.Duration,
) (bool, error) {
	wait := &jobWait{client: c, endpointID: endpointID, job: job, minIteration: minIteration}

	// Swarm replaces a failed task after the restart delay, unless the restart policy rules out another
	// attempt (condition none, max_attempts reached).
	wait.retryWait = 5 * time.Second
	if delay := job.Spec.TaskTemplate.RestartPolicy.Delay; delay != nil {
		wait.retryWait = time.Duration(*delay)
	}

	wait.retryWait += 2 * c.pollInterval

	return pollUntil(ctx, timeout, c.pollInterval, func() error { return wait.poll(ctx) })
}

// jobWait is the state of waiting for a job iteration that is kept between polls.
type jobWait struct {
	client       *Client
	endpointID   int
	job          portainerService
	minIteration uint64
	retryWait    time.Duration

	// Nodes change rarely, so the node list is refreshed at most every 30 seconds.
	activeNodes  map[string]bool
	nodesFetched time.Time

	// The job iteration to wait for, once it reached minIteration. The plugin's own update is the only
	// change expected meanwhile, so the iteration is not looked up again after that.
	iteration      uint64
	iterationFound bool

	// Tasks that have all ended, and since when they have not changed.
	endedTasks string
	endedSince time.Time
}

// poll checks the job once. It returns nil when the iteration completed, and a permanent error when
// waiting longer cannot help.
func (w *jobWait) poll(ctx context.Context) error {
	tasks, err := w.iterationTasks(ctx, true)
	if err != nil {
		return err
	}

	if err := jobCompletion(w.job, tasks); err != nil {
		return w.checkRetrying(tasks, err)
	}

	if w.job.Spec.Mode.GlobalJob != nil {
		if err := w.confirmGlobalJob(ctx, tasks); err != nil {
			return err
		}
	}

	log.Info().Msgf("Job %s completed", w.job.Spec.Name)

	return nil
}

// confirmGlobalJob looks at a global job that appears complete once more after a poll interval.
// Swarm may still be creating the tasks for some nodes when the tasks of other nodes have completed,
// so the job is complete only if no tasks were added meanwhile. The second look is part of the same
// poll, so it also happens when the timeout leaves no time for another poll, and it uses the same
// node list, so a node changing availability in between does not look like an added task.
func (w *jobWait) confirmGlobalJob(ctx context.Context, tasks []dockerTask) error {
	timer := time.NewTimer(w.client.pollInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	again, err := w.iterationTasks(ctx, false)
	if err != nil {
		return err
	}

	if err := jobCompletion(w.job, again); err != nil {
		return err
	}

	if taskNodes(again) != taskNodes(tasks) {
		return fmt.Errorf("job %s: tasks were added on more nodes", w.job.Spec.Name)
	}

	return nil
}

// iterationTasks returns the tasks of the job's current iteration once it reached minIteration.
// Tasks of a global job on drained or unavailable nodes are left out: they may never complete,
// and Swarm does not need them.
//
// The node list is refreshed when it is older than 30 seconds and refreshNodes is set.
func (w *jobWait) iterationTasks(ctx context.Context, refreshNodes bool) ([]dockerTask, error) {
	if !w.iterationFound {
		var current struct {
			JobStatus serviceJobStatus `json:"JobStatus"`
		}

		err := w.client.getJSON(ctx, w.client.serviceURL(w.endpointID, w.job.ID), &current)
		if err != nil {
			return nil, w.requestError(err)
		}

		iteration := current.JobStatus.JobIteration.Index
		if iteration < w.minIteration {
			return nil, fmt.Errorf("job %s: waiting for iteration %d to start (current: %d)", w.job.Spec.Name, w.minIteration, iteration)
		}

		w.iteration, w.iterationFound = iteration, true
	}

	iteration := w.iteration

	tasks, err := w.client.getTasks(ctx, w.endpointID, fmt.Sprintf(`{"service":[%q]}`, w.job.ID))
	if err != nil {
		return nil, w.requestError(err)
	}

	globalJob := w.job.Spec.Mode.GlobalJob != nil

	if globalJob && (w.activeNodes == nil || (refreshNodes && time.Since(w.nodesFetched) >= 30*time.Second)) {
		w.activeNodes, err = w.client.getActiveNodes(ctx, w.endpointID)
		if err != nil {
			return nil, w.requestError(err)
		}

		w.nodesFetched = time.Now()
	}

	var iterationTasks []dockerTask

	for _, task := range tasks {
		if task.JobIteration.Index == iteration && (!globalJob || w.activeNodes[task.NodeID]) {
			iterationTasks = append(iterationTasks, task)
		}
	}

	return iterationTasks, nil
}

// checkRetrying returns the incomplete job's error, as a permanent error once every task has ended
// and Swarm has not added a task for longer than the restart delay: Swarm has then given up.
func (w *jobWait) checkRetrying(tasks []dockerTask, err error) error {
	if len(tasks) == 0 || slices.ContainsFunc(tasks, taskInProgress) {
		w.endedTasks = ""

		return err
	}

	if states := taskStates(tasks); states != w.endedTasks {
		w.endedTasks, w.endedSince = states, time.Now()
	} else if time.Since(w.endedSince) > w.retryWait {
		return backoff.Permanent(fmt.Errorf("%w; Swarm is not retrying the job", err))
	}

	return err
}

// requestError returns a failed request's error, as a permanent error when retrying cannot help.
func (w *jobWait) requestError(err error) error {
	if permanentError(err) {
		return backoff.Permanent(fmt.Errorf("job %s: %w", w.job.Spec.Name, err))
	}

	return err
}

// taskInProgress reports whether a task has not reached a final state yet.
func taskInProgress(task dockerTask) bool {
	switch task.Status.State {
	case "complete", "failed", "rejected", "shutdown", "orphaned", "remove":
		return false
	}

	return true
}

// taskStates returns the sorted IDs and states of the tasks, to tell whether they changed between polls.
func taskStates(tasks []dockerTask) string {
	states := make([]string, 0, len(tasks))

	for _, task := range tasks {
		states = append(states, task.ID+"="+task.Status.State)
	}

	slices.Sort(states)

	return strings.Join(states, ",")
}

// jobCompletion returns nil when the given tasks of one job iteration show that the iteration completed.
func jobCompletion(job portainerService, tasks []dockerTask) error {
	completed := uint64(0)
	nodes := map[string]bool{}

	for _, task := range tasks {
		if _, seen := nodes[task.NodeID]; !seen {
			nodes[task.NodeID] = false
		}

		if task.Status.State == "complete" {
			completed++
			nodes[task.NodeID] = true
		}
	}

	if mode := job.Spec.Mode.ReplicatedJob; mode != nil {
		// Swarm defaults TotalCompletions to MaxConcurrent, which defaults to 1.
		required := mode.TotalCompletions
		if required == 0 {
			required = max(mode.MaxConcurrent, 1)
		}

		if completed >= required {
			return nil
		}

		return fmt.Errorf("job %s: %d/%d tasks completed (%s)", job.Spec.Name, completed, required, describeTasks(tasks))
	}

	// A global job runs one task on every eligible node; it is complete when each node has a completed task.
	// No tasks may mean Swarm has not created them yet, so the job does not count as complete.
	if len(nodes) == 0 {
		return fmt.Errorf("job %s: no tasks on any ready, active node yet (check the placement constraints and node availability)", job.Spec.Name)
	}

	for _, done := range nodes {
		if !done {
			return fmt.Errorf("job %s: %d task(s) completed on %d node(s) (%s)", job.Spec.Name, completed, len(nodes), describeTasks(tasks))
		}
	}

	return nil
}

// taskNodes returns the sorted, comma-separated IDs of the nodes the tasks run on.
func taskNodes(tasks []dockerTask) string {
	nodes := make([]string, 0, len(tasks))

	for _, task := range tasks {
		nodes = append(nodes, task.NodeID)
	}

	slices.Sort(nodes)

	return strings.Join(slices.Compact(nodes), ",")
}

// describeTasks summarizes task states and errors for job error messages.
func describeTasks(tasks []dockerTask) string {
	if len(tasks) == 0 {
		return "no tasks yet"
	}

	descriptions := make([]string, 0, len(tasks))

	for _, task := range tasks {
		description := task.Status.State
		if task.Status.Err != "" {
			description += ": " + task.Status.Err
		}

		descriptions = append(descriptions, description)
	}

	return "tasks: " + strings.Join(descriptions, "; ")
}

// getTasks lists the Swarm tasks matching the given filters JSON.
func (c *Client) getTasks(ctx context.Context, endpointID int, filters string) ([]dockerTask, error) {
	var tasks []dockerTask

	err := c.getJSON(ctx, c.dockerURL(endpointID, "/tasks?filters="+url.QueryEscape(filters)), &tasks)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// getActiveNodes returns the IDs of Swarm nodes that are ready and available for new tasks.
func (c *Client) getActiveNodes(ctx context.Context, endpointID int) (map[string]bool, error) {
	var nodes []struct {
		ID   string `json:"ID"`
		Spec struct {
			Availability string `json:"Availability"`
		} `json:"Spec"`
		Status struct {
			State string `json:"State"`
		} `json:"Status"`
	}

	err := c.getJSON(ctx, c.dockerURL(endpointID, "/nodes"), &nodes)
	if err != nil {
		return nil, err
	}

	active := make(map[string]bool, len(nodes))

	for _, node := range nodes {
		if node.Status.State == "ready" && node.Spec.Availability == "active" {
			active[node.ID] = true
		}
	}

	return active, nil
}
