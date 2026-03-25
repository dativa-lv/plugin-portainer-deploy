// Copyright (c) 2020, the Drone Plugins project authors.
// Copyright (c) 2021, Robert Kaussow <mail@thegeeklab.de>

// Use of this source code is governed by an Apache 2.0 license that can be
// found in the LICENSE file.

// Package plugin implements the Portainer deploy plugin logic.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

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

	if p.Settings.StackPath == "" {
		return errors.New("stack file path is required")
	}

	if p.Settings.RunningCheck {
		if p.Settings.RunningTimeout == "" {
			return errors.New("running-check-timeout is required when running-check is enabled")
		}

		_, err := time.ParseDuration(p.Settings.RunningTimeout)
		if err != nil {
			return fmt.Errorf("invalid running-check-timeout %q: %w", p.Settings.RunningTimeout, err)
		}
	}

	if p.Settings.HealthCheck {
		return p.validateHealthCheck()
	}

	return nil
}

func (p *Plugin) validateHealthCheck() error {
	if p.Settings.HealthCheckURL == "" {
		return errors.New("health-check-url is required when health-check is enabled")
	}

	healthURL, err := url.ParseRequestURI(p.Settings.HealthCheckURL)
	if err != nil || healthURL.Scheme == "" || healthURL.Host == "" {
		return fmt.Errorf("health-check-url must be a full URL, got %q", p.Settings.HealthCheckURL)
	}

	_, err = time.ParseDuration(p.Settings.HealthCheckTimeout)
	if err != nil {
		return fmt.Errorf("invalid health-check-timeout %q: %w", p.Settings.HealthCheckTimeout, err)
	}

	return nil
}

// Execute performs the main functionality of the Plugin.
func (p *Plugin) Execute(ctx context.Context) error {
	if err := p.Validate(); err != nil { //nolint:noinlineerr
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
		APIKey:             p.Settings.APIKey,
		ServerURL:          p.Settings.ServerURL,
		ServerEnvironment:  p.Settings.ServerEnvironment,
		StackName:          p.Settings.StackName,
		StackPath:          p.Settings.StackPath,
		Prune:              p.Settings.Prune,
		Teams:              teams,
		RunningCheck:       p.Settings.RunningCheck,
		RunningTimeout:     p.Settings.RunningTimeout,
		HealthCheck:        p.Settings.HealthCheck,
		HealthCheckURL:     p.Settings.HealthCheckURL,
		HealthCheckTimeout: p.Settings.HealthCheckTimeout,
	}

	// Initialize Portainer client
	client, err := NewClient(p, config)
	if err != nil {
		return fmt.Errorf("failed to initialize client: %w", err)
	}

	// Retrieve endpoint ID by name
	endpointID, err := client.GetEndpointID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get endpoint ID for endpoint name %s: %w", p.Settings.ServerEnvironment, err)
	}

	swarmID, err := client.GetSwarmID(ctx, endpointID)
	if err != nil {
		return fmt.Errorf("failed to get cluster ID for endpoint ID %d: %w", endpointID, err)
	}

	stackID, err := client.CreateOrUpdateStack(ctx, endpointID, swarmID)
	if err != nil {
		return fmt.Errorf("failed to create or update stack %s: %w", p.Settings.StackName, err)
	}

	_, err = client.UpdateResourceControl(ctx, stackID)
	if err != nil {
		return fmt.Errorf("failed to update resource control for stack %d: %w", stackID, err)
	}

	log.Info().Msgf("Successfully set %v teams to %s stack.\n", p.Settings.Teams, p.Settings.StackName)

	if p.Settings.RunningCheck {
		timeout, _ := time.ParseDuration(p.Settings.RunningTimeout)
		log.Info().Msgf("Stack deployed; waiting for stack %s to be Running (timeout: %s)...", p.Settings.StackName, timeout)

		err := client.WaitForStackRunning(ctx, endpointID, timeout)
		if err != nil {
			return fmt.Errorf("running check failed: %w", err)
		}
	}

	if p.Settings.HealthCheck {
		timeout, _ := time.ParseDuration(p.Settings.HealthCheckTimeout)
		log.Info().Msgf("Stack %s is Running; checking service health at %s (timeout: %s)...", p.Settings.StackName, p.Settings.HealthCheckURL, timeout)

		err := client.CheckHealth(ctx, p.Settings.HealthCheckURL, timeout)
		if err != nil {
			return fmt.Errorf("health check failed: %w", err)
		}
	}

	return nil
}
