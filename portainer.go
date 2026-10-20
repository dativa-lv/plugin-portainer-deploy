// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rs/zerolog/log"
)

// The Client represents a connection to a remote service.
type Client struct {
	plugin            *Plugin
	httpClient        *http.Client
	pollInterval      time.Duration
	ServerURL         string
	ServerEnvironment string
	APIKey            string
	StackName         string
	StackPath         string
	ServiceName       string
	Prune             bool
	Teams             []string
	Public            bool
	RunningCheck      bool
	RunningTimeout    string // passed to the stack as the RUNNING_CHECK_TIMEOUT variable
	HealthCheck       bool
	HealthCheckURL    string
}

// ResourceControl represents the access control settings for a resource in Portainer.
type ResourceControl struct {
	AdministratorsOnly bool  `json:"administratorsOnly"`
	Public             bool  `json:"public"`
	Restricted         bool  `json:"restricted"`
	Teams              []int `json:"teams"`
}

// portainerEndpoint is the API response shape for /api/endpoints list entries.
type portainerEndpoint struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
}

// portainerSwarm is the API response shape for /api/endpoints/{id}/docker/swarm.
type portainerSwarm struct {
	ID string `json:"ID"`
}

// portainerStack is the API response shape for stack list entries and create/update responses.
type portainerStack struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
}

// portainerResourceCtrl is the ResourceControl block embedded in a single-stack response.
type portainerResourceCtrl struct {
	ID int `json:"Id"`
}

// portainerStackDetail is the full API response for a single stack (includes ResourceControl).
// Status and DeploymentStatus are reported by Portainer versions that deploy stacks asynchronously.
type portainerStackDetail struct {
	ID               int                         `json:"Id"`
	Name             string                      `json:"Name"`
	ResourceControl  portainerResourceCtrl       `json:"ResourceControl"`
	Status           int                         `json:"Status,omitempty"`
	DeploymentStatus []portainerDeploymentStatus `json:"DeploymentStatus,omitempty"`
}

// portainerDeploymentStatus is one status transition of the current stack deployment.
type portainerDeploymentStatus struct {
	Status  int    `json:"Status"`
	Time    int64  `json:"Time"`
	Message string `json:"Message,omitempty"`
}

// portainerTeam is the API response shape for team list entries.
type portainerTeam struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
}

// portainerService is the API response shape for stack service list entries.
type portainerService struct {
	ID   string `json:"ID"`
	Spec struct {
		Name         string `json:"Name"`
		TaskTemplate struct {
			RestartPolicy struct {
				// Delay is in nanoseconds; Swarm uses 5 seconds when it is not set.
				Delay *int64 `json:"Delay,omitempty"`
			} `json:"RestartPolicy"`
		} `json:"TaskTemplate"`
		// Mode is the Swarm service mode. Exactly one of the fields is set by Swarm.
		Mode struct {
			Replicated struct {
				Replicas int64 `json:"Replicas"`
			} `json:"Replicated"`
			ReplicatedJob *replicatedJobMode `json:"ReplicatedJob,omitempty"`
			GlobalJob     *struct{}          `json:"GlobalJob,omitempty"`
		} `json:"Mode"`
	} `json:"Spec"`
	JobStatus    serviceJobStatus `json:"JobStatus"`
	UpdateStatus struct {
		State string `json:"State"`
	} `json:"UpdateStatus"`
}

// swarmVersion is a Swarm object version or job iteration. Swarm omits Index when it is 0.
type swarmVersion struct {
	Index uint64 `json:"Index,omitempty"`
}

type replicatedJobMode struct {
	MaxConcurrent    uint64 `json:"MaxConcurrent"`
	TotalCompletions uint64 `json:"TotalCompletions"`
}

// serviceJobStatus is reported by Swarm for job services only.
type serviceJobStatus struct {
	JobIteration swarmVersion `json:"JobIteration"`
}

// isJob reports whether the service runs in replicated-job or global-job mode.
func (s portainerService) isJob() bool {
	return s.Spec.Mode.ReplicatedJob != nil || s.Spec.Mode.GlobalJob != nil
}

// Set constants for...
const (
	apiKeyHeader      = "X-API-Key" //nolint:gosec // G101: header name constant, not a credential
	contentTypeHeader = "Content-Type"
	contentTypeJSON   = "application/json"
	requestError      = "failed to create request: "
	decodeError       = "failed to decode response: "
	requestFailed     = "request failed: "
	serverError       = "server error: "
)

// NewClient creates a new instance of the Client, initializing it with the provided configuration.
func NewClient(plugin *Plugin, c Client) (*Client, error) {
	// Wrap the plugin HTTP client with a default timeout to prevent indefinite hangs.
	base := plugin.HTTPClient()
	httpClient := &http.Client{
		Transport:     base.Transport,
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
		Timeout:       30 * time.Second,
	}

	if base.Timeout > 0 {
		httpClient.Timeout = base.Timeout
	}

	return &Client{
		plugin:            plugin,
		httpClient:        httpClient,
		pollInterval:      5 * time.Second,
		ServerURL:         c.ServerURL,
		ServerEnvironment: c.ServerEnvironment,
		StackName:         c.StackName,
		StackPath:         c.StackPath,
		ServiceName:       c.ServiceName,
		APIKey:            c.APIKey,
		Prune:             c.Prune,
		Teams:             c.Teams,
		Public:            c.Public,
		RunningCheck:      c.RunningCheck,
		RunningTimeout:    c.RunningTimeout,
		HealthCheck:       c.HealthCheck,
		HealthCheckURL:    c.HealthCheckURL,
	}, nil
}

// ConvertToIntSlice converts a slice of strings to a slice of integers, skipping unparseable values.
func ConvertToIntSlice(strSlice []string) []int {
	var intSlice []int

	for _, str := range strSlice {
		val := strings.TrimSpace(strings.TrimSuffix(str, ","))

		teamID, err := strconv.Atoi(val)
		if err != nil {
			log.Debug().Msgf("Skipping unparseable team ID %q: %v", val, err)

			continue
		}

		intSlice = append(intSlice, teamID)
	}

	return intSlice
}

// GetEndpointID retrieves the ID of a Portainer endpoint given its name.
// A large limit is applied to reduce the chance of the target endpoint being on a later page;
// deployments with more than 100 environments may need to increase this value.
func (c *Client) GetEndpointID(ctx context.Context) (int, error) {
	var endpoints []portainerEndpoint

	err := c.getJSON(ctx, fmt.Sprintf("%s/api/endpoints?search=%s&limit=100", c.ServerURL, url.QueryEscape(c.ServerEnvironment)), &endpoints)
	if err != nil {
		return 0, err
	}

	for _, ep := range endpoints {
		if ep.Name == c.ServerEnvironment {
			log.Info().Msgf("Successfully retrieved Endpoint ID: %d", ep.ID)

			return ep.ID, nil
		}
	}

	return 0, fmt.Errorf("endpoint %q not found", c.ServerEnvironment)
}

// GetSwarmID retrieves the Swarm cluster ID for a given Portainer endpoint.
func (c *Client) GetSwarmID(ctx context.Context, endpointID int) (string, error) {
	var swarm portainerSwarm

	err := c.getJSON(ctx, fmt.Sprintf("%s/api/endpoints/%d/docker/swarm", c.ServerURL, endpointID), &swarm)
	if err != nil {
		return "", err
	}

	if swarm.ID == "" {
		return "", errors.New("could not find Cluster ID")
	}

	log.Info().Msgf("Successfully retrieved Swarm ID: %s", swarm.ID)

	return swarm.ID, nil
}

// CreateOrUpdateStack creates or updates a stack in Portainer.
func (c *Client) CreateOrUpdateStack(ctx context.Context, endpointID int, swarmID string) (int, error) {
	filters := fmt.Sprintf(`{"SwarmID":"%s"}`, swarmID)

	var stacks []portainerStack

	err := c.getJSON(ctx, fmt.Sprintf("%s/api/stacks?filters=%s", c.ServerURL, url.QueryEscape(filters)), &stacks)
	if err != nil {
		return 0, err
	}

	for _, stack := range stacks {
		if stack.Name == c.StackName {
			log.Info().Msgf("Stack %s already exists, updating...", c.StackName)

			return c.UpdateExistingStack(ctx, stack.ID, endpointID)
		}
	}

	return c.CreateNewStack(ctx, endpointID, swarmID)
}

// CreateNewStack creates a new stack in Portainer.
func (c *Client) CreateNewStack(ctx context.Context, endpointID int, swarmID string) (int, error) {
	yamlContent, err := c.readYAMLAsString()
	if err != nil {
		return 0, fmt.Errorf("failed to read YAML file: %w", err)
	}

	payload := map[string]any{
		"Name":             c.StackName,
		"SwarmID":          swarmID,
		"StackFileContent": yamlContent,
		"Prune":            c.Prune,
		"ForceRecreate":    true,
		"PullImage":        true,
		"ForceUpdate":      true,
		"ForcePullImage":   true,
		"Env":              c.stackEnv(),
	}

	var created portainerStack

	err = c.sendJSON(ctx, http.MethodPost, fmt.Sprintf("%s/api/stacks/create/swarm/string?endpointId=%d", c.ServerURL, endpointID),
		payload, nil, &created)
	if err != nil {
		return 0, fmt.Errorf("failed to create stack: %w", err)
	}

	if created.ID == 0 {
		return 0, errors.New("could not retrieve stack ID from response")
	}

	log.Info().Msgf("Stack %s successfully created with ID: %d", c.StackName, created.ID)

	return created.ID, nil
}

// UpdateExistingStack updates an existing stack in Portainer.
func (c *Client) UpdateExistingStack(ctx context.Context, stackID int, endpointID int) (int, error) {
	yamlContent, err := c.readYAMLAsString()
	if err != nil {
		return 0, fmt.Errorf("failed to read YAML file: %w", err)
	}

	payload := map[string]any{
		"Env":              c.stackEnv(),
		"Prune":            c.Prune,
		"StackFileContent": yamlContent,
		"ForceRecreate":    true,
		"PullImage":        true,
		"ForceUpdate":      true,
		"ForcePullImage":   true,
	}

	var updated portainerStack

	err = c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/api/stacks/%d?endpointId=%d", c.ServerURL, stackID, endpointID),
		payload, nil, &updated)
	if err != nil {
		return 0, fmt.Errorf("failed to update stack: %w", err)
	}

	log.Debug().Msgf("Update response: %v", updated)
	log.Info().Msgf("Successfully %s updated with ID: %d", c.StackName, stackID)

	return stackID, nil
}

// Stack statuses reported by Portainer. Deploying and Error exist only in Portainer versions that
// deploy stacks asynchronously.
const (
	stackStatusDeploying = 3
	stackStatusError     = 4
)

// WaitForStackDeployment waits until Portainer finishes deploying the stack and returns Portainer's
// error when the deployment failed. Portainer versions that deploy stacks synchronously never
// report the deploying or error status, so the first poll returns immediately.
//
// Portainer sets the deploying status in the same transaction that handles the create or update
// request, before it responds, so the first poll cannot see the status of the previous deployment.
func (c *Client) WaitForStackDeployment(ctx context.Context, stackID int, timeout time.Duration) error {
	// Server and network errors may go away, e.g. while Portainer restarts, but not for long.
	var failingSince time.Time

	operation := func() error {
		detail, err := c.getStack(ctx, stackID)
		if err != nil {
			if permanentError(err) {
				return backoff.Permanent(err)
			}

			if failingSince.IsZero() {
				failingSince = time.Now()
			} else if time.Since(failingSince) > time.Minute {
				return backoff.Permanent(fmt.Errorf("requests failed for over a minute: %w", err))
			}

			return err
		}

		failingSince = time.Time{}

		switch detail.Status {
		case stackStatusDeploying:
			return fmt.Errorf("stack %s: deployment still in progress", c.StackName)
		case stackStatusError:
			// The latest error entry of the deployment carries Portainer's error message.
			message := "unknown error"

			for _, status := range slices.Backward(detail.DeploymentStatus) {
				if status.Status == stackStatusError && status.Message != "" {
					message = status.Message

					break
				}
			}

			return backoff.Permanent(fmt.Errorf(
				"portainer reported a deployment error: %s (Portainer restored the previous stack file)", message))
		}

		if len(detail.DeploymentStatus) == 0 {
			log.Debug().Msgf("Portainer reported no deployment status for stack %s; assuming synchronous deploy", c.StackName)
		} else {
			log.Info().Msgf("Portainer finished deploying stack %s", c.StackName)
		}

		return nil
	}

	timedOut, err := pollUntil(ctx, timeout, c.pollInterval, operation)
	if timedOut {
		return fmt.Errorf("portainer did not finish deploying within %s (deploy-timeout): %w", timeout, err)
	}

	return err
}

// UpdateResourceControl updates the access control settings for a resource in Portainer.
func (c *Client) UpdateResourceControl(ctx context.Context, stackID int) (string, error) {
	resourceID, err := c.GetResourceID(ctx, stackID)
	if err != nil {
		return "", fmt.Errorf("failed to get resource ID for stack: %w", err)
	}

	var teamIDs []string

	for _, teamName := range c.Teams {
		teamID, err := c.GetTeamIDByName(ctx, teamName)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve team ID for %s: %w", teamName, err)
		}

		if teamID != "" {
			teamIDs = append(teamIDs, teamID)
		}
	}

	log.Debug().Msgf("Team IDs: %v", teamIDs)

	var resourceControl ResourceControl

	switch {
	case c.Public:
		resourceControl = ResourceControl{
			AdministratorsOnly: false,
			Public:             true,
			Restricted:         false,
			Teams:              []int{},
		}
	case len(teamIDs) > 0:
		resourceControl = ResourceControl{
			AdministratorsOnly: false,
			Public:             false,
			Restricted:         true,
			Teams:              ConvertToIntSlice(teamIDs),
		}
	default:
		resourceControl = ResourceControl{
			AdministratorsOnly: true,
			Public:             false,
			Restricted:         false,
			Teams:              []int{},
		}
	}

	jsonData, err := json.Marshal(resourceControl)
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}

	log.Debug().Msgf("Full JSON Payload: %s", string(jsonData))

	_, err = c.SendPutRequest(ctx, resourceID, jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to update resource control: %w", err)
	}

	return "", nil
}

// GetResourceID retrieves the ResourceControl ID for a given stack in Portainer.
func (c *Client) GetResourceID(ctx context.Context, stackID int) (string, error) {
	detail, err := c.getStack(ctx, stackID)
	if err != nil {
		return "", err
	}

	if detail.ResourceControl.ID == 0 {
		return "", fmt.Errorf("ResourceControl not found for stack %d", stackID)
	}

	return strconv.Itoa(detail.ResourceControl.ID), nil
}

// GetTeamIDByName retrieves the ID of a team in Portainer given its name.
func (c *Client) GetTeamIDByName(ctx context.Context, teamName string) (string, error) {
	var teams []portainerTeam

	err := c.getJSON(ctx, c.ServerURL+"/api/teams", &teams)
	if err != nil {
		return "", err
	}

	target := strings.TrimSpace(teamName)
	for _, team := range teams {
		if team.Name == target {
			return strconv.Itoa(team.ID), nil
		}
	}

	return "", nil
}

// SendPutRequest sends a PUT request to the Portainer API.
func (c *Client) SendPutRequest(ctx context.Context, resourceID string, jsonData []byte) (string, error) {
	err := c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/api/resource_controls/%s", c.ServerURL, resourceID),
		json.RawMessage(jsonData), nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to update resource control: %w", err)
	}

	return "", nil
}

// dockerTask holds the subset of a Docker Swarm task response used for running-state and job checks.
type dockerTask struct {
	ID           string           `json:"ID"`
	ServiceID    string           `json:"ServiceID"`
	NodeID       string           `json:"NodeID"`
	JobIteration swarmVersion     `json:"JobIteration"`
	Status       dockerTaskStatus `json:"Status"`
}

type dockerTaskStatus struct {
	State string `json:"State"`
	Err   string `json:"Err,omitempty"`
}

// WaitForStackRunning polls the Docker Swarm tasks API until every service of the stack, except job
// services, has its desired replicas running and no update in progress, or until timeout elapses
// (zero means no limit). Runs every 5 seconds.
func (c *Client) WaitForStackRunning(ctx context.Context, endpointID int, timeout time.Duration) error {
	operation := func() error {
		// Get services in the stack
		services, err := c.getStackServices(ctx, endpointID)
		if err != nil {
			return err
		}

		// Get all running tasks for the stack
		filters := fmt.Sprintf(`{"label":["com.docker.stack.namespace=%s"],"desired-state":["running"]}`, c.StackName)

		tasks, err := c.getTasks(ctx, endpointID, filters)
		if err != nil {
			return err
		}

		// Verify each service has desired replicas running and update is complete
		for _, service := range services {
			// Job tasks run to completion instead of staying running. Jobs are checked before the
			// running check: by UpdateStackServices in service-update mode and by WaitForStackJobs
			// in stack deploys.
			if service.isJob() {
				continue
			}

			desiredReplicas := service.Spec.Mode.Replicated.Replicas
			runningCount := int64(0)

			for _, task := range tasks {
				if task.ServiceID == service.ID && task.Status.State == "running" {
					runningCount++
				}
			}

			if runningCount < desiredReplicas {
				return fmt.Errorf(
					"service %s: %d/%d replicas running",
					service.Spec.Name, runningCount, desiredReplicas)
			}

			if service.UpdateStatus.State != "" && service.UpdateStatus.State != "completed" {
				return fmt.Errorf(
					"service %s: update in progress (state: %s)",
					service.Spec.Name, service.UpdateStatus.State)
			}
		}

		log.Info().Msgf("Stack %s is running", c.StackName)

		return nil
	}

	timedOut, err := pollUntil(ctx, timeout, c.pollInterval, operation)
	if timedOut {
		return fmt.Errorf("stack %s was not running within %s: %w", c.StackName, timeout, err)
	}

	return err
}

// CheckHealth polls healthURL until it responds with HTTP 2xx within timeout.
// If the response Content-Type is application/health+json, a "fail" status body is also treated as unhealthy.
// runs every 5 seconds.
func (c *Client) CheckHealth(ctx context.Context, healthURL string, timeout time.Duration) error {
	operation := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create health check request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("health check request failed: %w", err)
		}

		defer resp.Body.Close() //nolint:errcheck

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		// Parse structured status when the server speaks application/health+json.
		if strings.Contains(resp.Header.Get(contentTypeHeader), "application/health+json") {
			var hj struct {
				Status string `json:"status"`
			}

			jsonErr := json.Unmarshal(body, &hj)
			if jsonErr == nil && hj.Status == "fail" {
				return errors.New("health endpoint reported status: fail")
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("health endpoint returned status %d: %s", resp.StatusCode, string(body))
		}

		log.Info().Msgf("Health check passed (HTTP %d)", resp.StatusCode)

		return nil
	}

	timedOut, err := pollUntil(ctx, timeout, c.pollInterval, operation)
	if timedOut {
		return fmt.Errorf("service was not healthy within %s: %w", timeout, err)
	}

	return err
}

// UpdateStackServices forces services in a stack to pull the latest image versions.
// With waitForJobs, each job must complete within jobTimeout (zero means no limit) before the next
// job or the other services are updated; otherwise jobs are started without waiting for them.
func (c *Client) UpdateStackServices(ctx context.Context, endpointID int, waitForJobs bool, jobTimeout time.Duration) error {
	services, err := c.listStackServices(ctx, endpointID)
	if err != nil {
		return err
	}

	selected := c.filterServicesByName(services)
	if len(selected) == 0 {
		if c.ServiceName != "" {
			return fmt.Errorf("service %q not found in stack %s", c.ServiceName, c.StackName)
		}

		return fmt.Errorf("no services found for stack %s", c.StackName)
	}

	jobs, regular := splitJobServices(selected)

	// Jobs (e.g. database migrations) run to completion before the other services are updated.
	if err := c.rerunJobs(ctx, endpointID, jobs, waitForJobs, jobTimeout); err != nil {
		return err
	}

	if len(regular) > 0 {
		logGroup("Update services in stack " + c.StackName)
	}

	for _, service := range regular {
		if err := c.forceUpdateService(ctx, endpointID, service.ID); err != nil {
			return fmt.Errorf("failed to update service %s: %w", service.Spec.Name, err)
		}

		log.Info().Msgf("Successfully updated service %s", service.Spec.Name)
	}

	log.Info().Msgf("Successfully updated %d service(s) in stack %s", len(selected), c.StackName)

	return nil
}

// splitJobServices separates job services from regular services, keeping their order.
func splitJobServices(services []portainerService) ([]portainerService, []portainerService) {
	var jobs, regular []portainerService

	for _, service := range services {
		if service.isJob() {
			jobs = append(jobs, service)
		} else {
			regular = append(regular, service)
		}
	}

	return jobs, regular
}

// rerunJobs starts a new iteration of each job and waits for it to complete, one job at a time.
func (c *Client) rerunJobs(ctx context.Context, endpointID int, jobs []portainerService, wait bool, timeout time.Duration) error {
	if len(jobs) == 0 {
		return nil
	}

	registries, err := c.getRegistries(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to list Portainer registries; job images are pulled without registry credentials")
	}

	for _, job := range jobs {
		logGroup("Run job " + job.Spec.Name)

		previousIteration, err := c.rerunJobService(ctx, endpointID, job.ID, registries)
		if err != nil {
			return fmt.Errorf("failed to update job %s: %w", job.Spec.Name, err)
		}

		if !wait {
			log.Warn().Msgf("Started job %s without waiting for it to complete (running-check is disabled); "+
				"the other services are updated right away", job.Spec.Name)

			continue
		}

		log.Info().Msgf("Started job %s; waiting for it to complete (timeout: %s)...", job.Spec.Name, timeout)

		timedOut, err := c.waitForJob(ctx, endpointID, job, previousIteration+1, timeout)
		if timedOut {
			return fmt.Errorf("job %s did not complete within %s (job-timeout): %w", job.Spec.Name, timeout, err)
		}

		if err != nil {
			return fmt.Errorf("job %s failed: %w", job.Spec.Name, err)
		}
	}

	return nil
}

func (c *Client) getStack(ctx context.Context, stackID int) (*portainerStackDetail, error) {
	var detail portainerStackDetail

	err := c.getJSON(ctx, fmt.Sprintf("%s/api/stacks/%d", c.ServerURL, stackID), &detail)
	if err != nil {
		return nil, err
	}

	return &detail, nil
}

func (c *Client) getStackServices(ctx context.Context, endpointID int) ([]portainerService, error) {
	filters := fmt.Sprintf(`{"label":["com.docker.stack.namespace=%s"]}`, c.StackName)

	var services []portainerService

	err := c.getJSON(ctx, c.dockerURL(endpointID, "/services?filters="+url.QueryEscape(filters)), &services)
	if err != nil {
		return nil, err
	}

	return services, nil
}

// statusError is returned for unsuccessful responses of the Portainer API.
type statusError struct {
	StatusCode int
	Body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf(serverError+"status code %d, body: %s", e.StatusCode, e.Body)
}

// pollUntil calls operation every interval until it succeeds, returns an error wrapped with
// backoff.Permanent, ctx is done or the timeout elapses; a timeout of zero means no limit. Unlike
// backoff.Retry, it makes the last attempt when the timeout elapses rather than up to one interval
// earlier, so short timeouts still get a full attempt. On timeout, timedOut is set and the last
// attempt's error is returned.
func pollUntil(ctx context.Context, timeout, interval time.Duration, operation func() error) (bool, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		err := operation()
		if err == nil {
			return false, nil
		}

		if permanent, ok := errors.AsType[*backoff.PermanentError](err); ok {
			return false, permanent.Unwrap()
		}

		wait := interval

		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return true, err
			}

			wait = min(wait, remaining)
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

// permanentError reports whether retrying a failed Portainer request cannot help: client errors other
// than timeouts, conflicts and rate limits (e.g. the resource was removed or the API key lost access),
// and responses that are not the expected JSON.
func permanentError(err error) bool {
	if status, ok := errors.AsType[*statusError](err); ok {
		switch status.StatusCode {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return false
		}

		return status.StatusCode >= 400 && status.StatusCode < 500
	}

	_, syntaxError := errors.AsType[*json.SyntaxError](err)
	_, typeError := errors.AsType[*json.UnmarshalTypeError](err)

	return syntaxError || typeError
}

// getJSON sends an authenticated GET request and decodes the JSON response into out.
// Numbers decoded into interface values are kept as json.Number, so they are re-encoded exactly.
func (c *Client) getJSON(ctx context.Context, requestURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("GET request uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf(requestFailed+"%w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return &statusError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()

	err = decoder.Decode(out)
	if err != nil {
		return fmt.Errorf(decodeError+"%w", err)
	}

	return nil
}

// sendJSON sends an authenticated request with a JSON body and, when out is not nil, decodes the
// JSON response into it. Non-2xx responses are returned as *statusError.
func (c *Client) sendJSON(ctx context.Context, method, requestURL string, payload any, header http.Header, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf(requestError+"%w", err)
	}

	// Request and response bodies are not logged: they contain stack and service environment variables.
	log.Debug().Msgf("%s request uri: %s", method, req.URL.String())

	maps.Copy(req.Header, header)

	req.Header.Set(apiKeyHeader, c.APIKey)
	req.Header.Set(contentTypeHeader, contentTypeJSON)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf(requestFailed+"%w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	log.Debug().Msgf("Response status: %s", resp.Status)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{StatusCode: resp.StatusCode, Body: string(responseBody)}
	}

	if out == nil {
		return nil
	}

	err = json.Unmarshal(responseBody, out)
	if err != nil {
		return fmt.Errorf(decodeError+"%w", err)
	}

	return nil
}

func (c *Client) filterServicesByName(services []portainerService) []portainerService {
	target := strings.TrimSpace(c.ServiceName)
	if target == "" {
		return services
	}

	selected := make([]portainerService, 0, 1)

	for _, service := range services {
		if service.Spec.Name == target || strings.HasSuffix(service.Spec.Name, "_"+target) {
			selected = append(selected, service)
		}
	}

	return selected
}

func (c *Client) forceUpdateService(ctx context.Context, endpointID int, serviceID string) error {
	payload := map[string]any{
		"serviceID": serviceID,
		"pullImage": true,
	}

	err := c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/api/endpoints/%d/forceupdateservice", c.ServerURL, endpointID),
		payload, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to force update service: %w", err)
	}

	return nil
}

func (c *Client) stackEnv() []map[string]string {
	env := []map[string]string{}

	if c.RunningCheck {
		env = append(env,
			map[string]string{"name": "RUNNING_CHECK", "value": "true"}, //nolint:goconst
			map[string]string{"name": "RUNNING_CHECK_TIMEOUT", "value": c.RunningTimeout},
		)
	}

	if c.HealthCheck {
		env = append(env,
			map[string]string{"name": "HEALTH_CHECK", "value": "true"},
			map[string]string{"name": "HEALTH_CHECK_URL", "value": c.HealthCheckURL},
		)
	}

	return env
}

// readYAMLAsString reads a YAML file and returns its content as a string.
func (c *Client) readYAMLAsString() (string, error) {
	data, err := os.ReadFile(c.StackPath)
	if err != nil {
		return "", fmt.Errorf("error reading YAML file: %w", err)
	}

	return string(data), nil
}
