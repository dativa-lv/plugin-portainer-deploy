// SPDX-License-Identifier: Apache-2.0

// Package plugin implements the Portainer deploy plugin logic.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"codeberg.org/woodpecker-plugins/go-plugin"
	"github.com/rs/zerolog/log"
)

// logGroup starts a collapsible output group in the Woodpecker web UI.
func logGroup(title string) {
	fmt.Println(plugin.LogGroupMarker + title) //nolint:forbidigo // group markers must be printed as plain lines
}

// Validate handles the settings validation of the plugin.
func (p *Plugin) Validate() error {
	if p.Settings.APIKey == "" {
		return errors.New("you must provide an API key")
	}

	if p.Settings.ServerURL == "" {
		return errors.New("portainer base url are required")
	}

	if p.Settings.ServerEnvironment == "" {
		return errors.New("portainer environment is required")
	}

	if p.Settings.StackName == "" {
		return errors.New("stack name is required")
	}

	if p.Settings.Public && strings.TrimSpace(p.Settings.Teams) != "" {
		return errors.New("public and teams cannot be used together")
	}

	if err := p.parseTimeouts(); err != nil {
		return err
	}

	if p.Settings.HealthCheck {
		return p.validateHealthCheck()
	}

	return nil
}

// parseTimeouts validates the timeout settings and stores them as durations. For all timeouts,
// zero means no limit.
func (p *Plugin) parseTimeouts() error {
	if p.Settings.RunningCheck && p.Settings.RunningTimeout == "" {
		return errors.New("running-check-timeout is required when running-check is enabled")
	}

	var err error

	if p.Settings.RunningTimeout != "" {
		p.Settings.runningTimeout, err = parseTimeout("running-check-timeout", p.Settings.RunningTimeout)
		if err != nil {
			return err
		}
	}

	// job-timeout defaults to running-check-timeout. It is validated even without the running check,
	// where it is not used, so that enabling the running check does not reveal a bad value.
	p.Settings.jobTimeout = p.Settings.runningTimeout

	if p.Settings.JobTimeout != "" {
		p.Settings.jobTimeout, err = parseTimeout("job-timeout", p.Settings.JobTimeout)
		if err != nil {
			return err
		}
	}

	p.Settings.deployTimeout = defaultDeployTimeout

	if p.Settings.DeployTimeout != "" {
		p.Settings.deployTimeout, err = parseTimeout("deploy-timeout", p.Settings.DeployTimeout)
		if err != nil {
			return err
		}
	}

	return nil
}

// parseTimeout parses a timeout setting: a Go duration that is not negative, where zero means no limit.
func parseTimeout(name, value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}

	if timeout < 0 {
		return 0, fmt.Errorf("invalid %s %q: must not be negative", name, value)
	}

	return timeout, nil
}

func (p *Plugin) validateHealthCheck() error {
	if p.Settings.HealthCheckURL == "" {
		return errors.New("health-check-url is required when health-check is enabled")
	}

	healthURL, err := url.ParseRequestURI(p.Settings.HealthCheckURL)
	if err != nil || healthURL.Scheme == "" || healthURL.Host == "" {
		return fmt.Errorf("health-check-url must be a full URL, got %q", p.Settings.HealthCheckURL)
	}

	p.Settings.healthCheckTimeout, err = parseTimeout("health-check-timeout", p.Settings.HealthCheckTimeout)

	return err
}

// Execute performs the main functionality of the Plugin.
func (p *Plugin) Execute(ctx context.Context) error {
	if err := p.Validate(); err != nil {
		return err
	}

	// Parse Teams once here so the client receives a clean []string.
	var teams []string

	for t := range strings.SplitSeq(p.Settings.Teams, ",") {
		if trimmed := strings.TrimSpace(t); trimmed != "" {
			teams = append(teams, trimmed)
		}
	}

	// Prepare the client configuration
	config := Client{
		APIKey:            p.Settings.APIKey,
		ServerURL:         p.Settings.ServerURL,
		ServerEnvironment: p.Settings.ServerEnvironment,
		StackName:         p.Settings.StackName,
		StackPath:         p.Settings.StackPath,
		ServiceName:       p.Settings.ServiceName,
		Prune:             p.Settings.Prune,
		Teams:             teams,
		Public:            p.Settings.Public,
		RunningCheck:      p.Settings.RunningCheck,
		RunningTimeout:    p.Settings.RunningTimeout,
		HealthCheck:       p.Settings.HealthCheck,
		HealthCheckURL:    p.Settings.HealthCheckURL,
	}

	// Initialize Portainer client
	client, err := NewClient(p, config)
	if err != nil {
		return fmt.Errorf("failed to initialize client: %w", err)
	}

	logGroup("Connect to Portainer environment " + p.Settings.ServerEnvironment)

	// Retrieve endpoint ID by name
	endpointID, err := client.GetEndpointID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get endpoint ID for endpoint name %s: %w", p.Settings.ServerEnvironment, err)
	}

	if p.Settings.StackPath == "" {
		// Waiting for jobs is part of the running check, as in stack deploys.
		if err := client.UpdateStackServices(ctx, endpointID, p.Settings.RunningCheck, p.Settings.jobTimeout); err != nil {
			return fmt.Errorf("failed to update services for stack %s: %w", p.Settings.StackName, err)
		}

		return p.runPostUpdateChecks(ctx, client, endpointID, "Services updated")
	}

	swarmID, err := client.GetSwarmID(ctx, endpointID)
	if err != nil {
		return fmt.Errorf("failed to get cluster ID for endpoint ID %d: %w", endpointID, err)
	}

	logGroup("Deploy stack " + p.Settings.StackName)

	// Job iterations before the deploy tell which jobs the deploy started. Without them, jobs the
	// deploy did not start could not be told apart, so the jobs are not checked. A stack file without
	// jobs needs neither.
	var jobIterations map[string]uint64

	waitForJobs := p.Settings.RunningCheck && client.StackFileDeclaresJobs()

	if waitForJobs {
		jobIterations, err = client.StackJobIterations(ctx, endpointID)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to read job iterations before the deploy; job services will not be checked")

			waitForJobs = false
		}
	}

	stackID, err := client.CreateOrUpdateStack(ctx, endpointID, swarmID)
	if err != nil {
		return fmt.Errorf("failed to create or update stack %s: %w", p.Settings.StackName, err)
	}

	if _, err := client.UpdateResourceControl(ctx, stackID); err != nil {
		return fmt.Errorf("failed to update resource control for stack %d: %w", stackID, err)
	}

	if p.Settings.Public {
		log.Info().Msgf("Successfully set %s stack to public.\n", p.Settings.StackName)
	} else {
		log.Info().Msgf("Successfully set %v teams to %s stack.\n", p.Settings.Teams, p.Settings.StackName)
	}

	if err := client.WaitForStackDeployment(ctx, stackID, p.Settings.deployTimeout); err != nil {
		return fmt.Errorf("failed to deploy stack %s: %w", p.Settings.StackName, err)
	}

	if waitForJobs {
		// Jobs get their own log groups.
		if err := client.WaitForStackJobs(ctx, endpointID, jobIterations, p.Settings.jobTimeout); err != nil {
			return fmt.Errorf("running check failed: %w", err)
		}
	}

	return p.runPostUpdateChecks(ctx, client, endpointID, "Stack deployed")
}

func (p *Plugin) runPostUpdateChecks(ctx context.Context, client *Client, endpointID int, prefix string) error {
	if p.Settings.RunningCheck {
		if err := p.runRunningCheck(ctx, client, endpointID, prefix); err != nil {
			return err
		}
	}

	if p.Settings.HealthCheck {
		if err := p.runHealthCheck(ctx, client, prefix); err != nil {
			return err
		}
	}

	return nil
}

func (p *Plugin) runRunningCheck(ctx context.Context, client *Client, endpointID int, prefix string) error {
	logGroup("Running check")

	log.Info().Msgf("%s; waiting for stack %s to be Running (timeout: %s)...", prefix, p.Settings.StackName, p.Settings.runningTimeout)

	if err := client.WaitForStackRunning(ctx, endpointID, p.Settings.runningTimeout); err != nil {
		return fmt.Errorf("running check failed: %w", err)
	}

	return nil
}

func (p *Plugin) runHealthCheck(ctx context.Context, client *Client, prefix string) error {
	logGroup("Health check")

	log.Info().Msgf("%s; checking service health at %s (timeout: %s)...", prefix, p.Settings.HealthCheckURL, p.Settings.healthCheckTimeout)

	if err := client.CheckHealth(ctx, p.Settings.HealthCheckURL, p.Settings.healthCheckTimeout); err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	return nil
}
