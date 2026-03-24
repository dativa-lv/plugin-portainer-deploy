package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// The Client represents a connection to a remote service.
type Client struct {
	plugin             *Plugin
	httpClient         *http.Client
	ServerURL          string
	ServerEnvironment  string
	APIKey             string
	StackName          string
	StackPath          string
	Prune              bool
	Teams              []string
	RunningCheck       bool
	RunningTimeout     string
	HealthCheck        bool
	HealthCheckURL     string
	HealthCheckTimeout string
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
type portainerStackDetail struct {
	ID              int                   `json:"Id"`
	Name            string                `json:"Name"`
	ResourceControl portainerResourceCtrl `json:"ResourceControl"`
}

// portainerTeam is the API response shape for team list entries.
type portainerTeam struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
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
		plugin:             plugin,
		httpClient:         httpClient,
		ServerURL:          c.ServerURL,
		ServerEnvironment:  c.ServerEnvironment,
		StackName:          c.StackName,
		StackPath:          c.StackPath,
		APIKey:             c.APIKey,
		Prune:              c.Prune,
		Teams:              c.Teams,
		RunningCheck:       c.RunningCheck,
		RunningTimeout:     c.RunningTimeout,
		HealthCheck:        c.HealthCheck,
		HealthCheckURL:     c.HealthCheckURL,
		HealthCheckTimeout: c.HealthCheckTimeout,
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
	encodedEndpoint := url.QueryEscape(c.ServerEnvironment)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/endpoints?search=%s&limit=100", c.ServerURL, encodedEndpoint), nil)
	if err != nil {
		return 0, fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("Request endpointID uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return 0, fmt.Errorf(serverError+"status code %d, body: %s", resp.StatusCode, string(body))
	}

	var endpoints []portainerEndpoint

	err = json.NewDecoder(resp.Body).Decode(&endpoints)
	if err != nil {
		return 0, fmt.Errorf(decodeError+"%w", err)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/endpoints/%d/docker/swarm", c.ServerURL, endpointID), nil)
	if err != nil {
		return "", fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("Request swarmID uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return "", fmt.Errorf(serverError+"status code %d, body: %s", resp.StatusCode, string(body))
	}

	var swarm portainerSwarm

	err = json.NewDecoder(resp.Body).Decode(&swarm)
	if err != nil {
		return "", fmt.Errorf(decodeError+"%w", err)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/stacks?filters=%s", c.ServerURL, url.QueryEscape(filters)), nil)
	if err != nil {
		return 0, fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("Request stacks filter uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return 0, fmt.Errorf(serverError+"status code %d, body: %s", resp.StatusCode, string(body))
	}

	var stacks []portainerStack

	err = json.NewDecoder(resp.Body).Decode(&stacks)
	if err != nil {
		return 0, fmt.Errorf(decodeError+"%w", err)
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

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	log.Debug().Msgf("Full JSON Payload: %s", string(payloadBytes))

	stackURL := fmt.Sprintf("%s/api/stacks/create/swarm/string?endpointId=%d", c.ServerURL, endpointID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stackURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return 0, fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("POST request uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)
	req.Header.Set(contentTypeHeader, contentTypeJSON)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body: %w", err)
	}

	log.Debug().Msgf("Response Status: %s", resp.Status)
	log.Debug().Msgf("Response Body: %s", string(responseBody))

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed to create stack, status code: %d, response: %s", resp.StatusCode, string(responseBody))
	}

	var created portainerStack

	err = json.Unmarshal(responseBody, &created)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal response: %w", err)
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

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	stackURL := fmt.Sprintf("%s/api/stacks/%d?endpointId=%d", c.ServerURL, stackID, endpointID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, stackURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return 0, fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("PUT request uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)
	req.Header.Set(contentTypeHeader, contentTypeJSON)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed to update stack, status code: %d, response: %s", resp.StatusCode, string(responseBody))
	}

	var updated portainerStack

	err = json.Unmarshal(responseBody, &updated)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	log.Debug().Msgf("Update response: %v", updated)
	log.Info().Msgf("Successfully %s updated with ID: %d", c.StackName, stackID)

	return stackID, nil
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
	if len(teamIDs) > 0 {
		resourceControl = ResourceControl{
			AdministratorsOnly: false,
			Public:             false,
			Restricted:         true,
			Teams:              ConvertToIntSlice(teamIDs),
		}
	} else {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/stacks/%d", c.ServerURL, stackID), nil)
	if err != nil {
		return "", fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("GET ResourceID request uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return "", fmt.Errorf("status code %d, body: %s", resp.StatusCode, string(body))
	}

	var detail portainerStackDetail

	err = json.NewDecoder(resp.Body).Decode(&detail)
	if err != nil {
		return "", fmt.Errorf(decodeError+"%w", err)
	}

	if detail.ResourceControl.ID == 0 {
		return "", fmt.Errorf("ResourceControl not found for stack %d", stackID)
	}

	return strconv.Itoa(detail.ResourceControl.ID), nil
}

// GetTeamIDByName retrieves the ID of a team in Portainer given its name.
func (c *Client) GetTeamIDByName(ctx context.Context, teamName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.ServerURL+"/api/teams", nil)
	if err != nil {
		return "", fmt.Errorf(requestError+"%w", err)
	}

	req.Header.Set(apiKeyHeader, c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(serverError+"status code %d, body: %s", resp.StatusCode, string(responseBody))
	}

	var teams []portainerTeam

	err = json.Unmarshal(responseBody, &teams)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
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
	putURL := fmt.Sprintf("%s/api/resource_controls/%s", c.ServerURL, resourceID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf(requestError+"%w", err)
	}

	log.Debug().Msgf("PUT Resource request uri: %s", req.URL.String())
	req.Header.Set(apiKeyHeader, c.APIKey)
	req.Header.Set(contentTypeHeader, contentTypeJSON)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf(requestFailed+"%w", err)
	}

	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			log.Warn().Err(cerr).Msg("failed to close response body")
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to update stack, status code: %d, response: %s", resp.StatusCode, string(responseBody))
	}

	log.Debug().Msgf("PUT Resource response: %s", string(responseBody))

	return "", nil
}

// dockerTask holds the subset of a Docker Swarm task response used for running-state checks.
type dockerTask struct {
	Status struct {
		State string `json:"State"`
	} `json:"Status"`
}

// pollUntil retries checkFn every interval until it returns nil (success),
// the timeout elapses, or ctx is cancelled.
func pollUntil(ctx context.Context, timeout, interval time.Duration, checkFn func() error) error {
	deadline := time.Now().Add(timeout)

	for {
		err := checkFn()
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			lastErr := checkFn()
			if lastErr == nil {
				return nil
			}

			return fmt.Errorf("timed out after %s: last error: %w", timeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// WaitForStackRunning polls the Docker Swarm tasks API until all tasks for the stack
// report State == "running", or until timeout elapses.
// runs every 5 seconds.
func (c *Client) WaitForStackRunning(ctx context.Context, endpointID int, timeout time.Duration) error {
	return pollUntil(ctx, timeout, 5*time.Second, func() error {
		filters := fmt.Sprintf(`{"label":["com.docker.stack.namespace=%s"],"desired-state":["running"]}`, c.StackName)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/api/endpoints/%d/docker/tasks?filters=%s",
				c.ServerURL, endpointID, url.QueryEscape(filters)), nil)
		if err != nil {
			return fmt.Errorf(requestError+"%w", err)
		}

		req.Header.Set(apiKeyHeader, c.APIKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf(requestFailed+"%w", err)
		}

		defer func() {
			cerr := resp.Body.Close()
			if cerr != nil {
				log.Warn().Err(cerr).Msg("failed to close response body")
			}
		}()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)

			return fmt.Errorf(serverError+"status code %d, body: %s", resp.StatusCode, string(body))
		}

		var tasks []dockerTask

		err = json.NewDecoder(resp.Body).Decode(&tasks)
		if err != nil {
			return fmt.Errorf(decodeError+"%w", err)
		}

		if len(tasks) == 0 {
			return fmt.Errorf("no tasks found for stack %s, waiting for scheduler", c.StackName)
		}

		for _, task := range tasks {
			if task.Status.State != "running" {
				return fmt.Errorf("task state is %q for stack %s", task.Status.State, c.StackName)
			}
		}

		log.Info().Msgf("Stack %s is running", c.StackName)

		return nil
	})
}

// CheckHealth polls healthURL until it responds with HTTP 2xx within timeout.
// If the response Content-Type is application/health+json, a "fail" status body is also treated as unhealthy.
// runs every 5 seconds.
func (c *Client) CheckHealth(ctx context.Context, healthURL string, timeout time.Duration) error {
	return pollUntil(ctx, timeout, 5*time.Second, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create health check request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("health check request failed: %w", err)
		}

		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				log.Warn().Err(cerr).Msg("failed to close response body")
			}
		}()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		// Parse structured status when the server speaks application/health+json.
		if strings.Contains(resp.Header.Get(contentTypeHeader), "application/health+json") {
			var hj struct {
				Status string `json:"status"`
			}
			if jsonErr := json.Unmarshal(body, &hj); jsonErr == nil && hj.Status == "fail" {
				return errors.New("health endpoint reported status: fail")
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("health endpoint returned status %d: %s", resp.StatusCode, string(body))
		}

		log.Info().Msgf("Health check passed (HTTP %d)", resp.StatusCode)

		return nil
	})
}

func (c *Client) stackEnv() []map[string]string {
	env := []map[string]string{}

	if c.RunningCheck {
		env = append(env,
			map[string]string{"name": "RUNNING_CHECK", "value": "true"},
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
