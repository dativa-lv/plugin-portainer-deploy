// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ConfigEntry is a Docker Swarm config managed in configs mode: the config name and the workspace
// file that holds its content.
type ConfigEntry struct {
	Name string `json:"name"`
	File string `json:"file"`
}

const (
	stackNamespaceLabel  = "com.docker.stack.namespace"
	stackStatusActive    = 1
	configResourceType   = 7
	configDeleteAttempts = 10
	configCreateAttempts = 3
)

// swarmConfig is the API response shape for Docker Swarm configs, decorated by Portainer.
type swarmConfig struct {
	ID   string `json:"ID"`
	Spec struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
		Data   string            `json:"Data"`
	} `json:"Spec"`
	Portainer struct {
		ResourceControl *portainerResourceCtrl `json:"ResourceControl"`
	} `json:"Portainer"`
}

// configService is the subset of a Docker Swarm service that tells which configs it uses.
type configService struct {
	Spec struct {
		Name         string            `json:"Name"`
		Labels       map[string]string `json:"Labels"`
		TaskTemplate struct {
			ContainerSpec struct {
				Configs []struct {
					ConfigID   string `json:"ConfigID"`
					ConfigName string `json:"ConfigName"`
				} `json:"Configs"`
			} `json:"ContainerSpec"`
		} `json:"TaskTemplate"`
	} `json:"Spec"`
}

// configPlan is what happens to one config: created when it does not exist, rotated when its
// content changed, otherwise only its access is updated.
type configPlan struct {
	entry    ConfigEntry
	newData  []byte
	oldData  []byte
	existing *swarmConfig
	changed  bool
}

// configsMode reports whether the plugin manages Swarm configs instead of deploying a stack.
func (p *Plugin) configsMode() bool {
	return strings.TrimSpace(p.Settings.ConfigsJSON) != ""
}

// validateConfigs validates the configs mode settings and parses the configs list.
func (p *Plugin) validateConfigs() error {
	if p.Settings.StackName != "" || p.Settings.StackPath != "" || p.Settings.ServiceName != "" || p.Settings.HealthCheck {
		return errors.New("configs cannot be combined with stack-name, stack-path, service-name or health-check; " +
			"deploy the stack in a separate step")
	}

	if err := json.Unmarshal([]byte(p.Settings.ConfigsJSON), &p.Settings.configEntries); err != nil {
		return fmt.Errorf("configs must be a list of {name, file} entries: %w", err)
	}

	if len(p.Settings.configEntries) == 0 {
		return errors.New("configs must contain at least one entry")
	}

	seen := make(map[string]bool, len(p.Settings.configEntries))

	for i, entry := range p.Settings.configEntries {
		if entry.Name == "" || entry.File == "" {
			return fmt.Errorf("configs[%d] must have both name and file", i)
		}

		if seen[entry.Name] {
			return fmt.Errorf("duplicate config name %q", entry.Name)
		}

		seen[entry.Name] = true
	}

	return p.parseTimeouts()
}

// syncConfigs creates the configs that do not exist and rotates the configs whose content changed.
// Swarm configs cannot be changed, so a changed config is deleted and created again; the stacks
// that use it are stopped first and started again afterwards. Every check that can fail is done
// before anything is changed, and on failure the stopped stacks are started again.
func (p *Plugin) syncConfigs(ctx context.Context, client *Client) error {
	newData := make(map[string][]byte, len(p.Settings.configEntries))

	for _, entry := range p.Settings.configEntries {
		data, err := os.ReadFile(entry.File)
		if err != nil {
			return fmt.Errorf("failed to read config file %q: %w", entry.File, err)
		}

		newData[entry.Name] = data
	}

	logGroup("Connect to Portainer environment " + p.Settings.ServerEnvironment)

	endpointID, err := client.GetEndpointID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get endpoint ID for endpoint name %s: %w", p.Settings.ServerEnvironment, err)
	}

	teamIDs, err := client.configTeamIDs(ctx)
	if err != nil {
		return err
	}

	logGroup("Check configs")

	plans, err := client.planConfigs(ctx, endpointID, p.Settings.configEntries, newData)
	if err != nil {
		return err
	}

	var rotated []*configPlan

	for _, plan := range plans {
		if plan.changed {
			rotated = append(rotated, plan)
		}
	}

	var stopped []portainerStack

	if len(rotated) > 0 {
		stacks, err := client.configStacks(ctx, endpointID, rotated)
		if err != nil {
			return err
		}

		logGroup("Stop stacks")

		stopped, err = client.stopStacks(ctx, endpointID, stacks)
		if err != nil {
			return client.recoverStacks(ctx, endpointID, stopped, err)
		}
	}

	logGroup("Update configs")

	for _, plan := range plans {
		if err := client.applyConfigPlan(ctx, endpointID, plan, teamIDs); err != nil {
			return client.recoverStacks(ctx, endpointID, stopped, err)
		}
	}

	if len(stopped) > 0 {
		logGroup("Start stacks")

		if err := client.startStacks(ctx, endpointID, stopped); err != nil {
			return err
		}
	}

	if p.Settings.RunningCheck {
		for _, stack := range stopped {
			logGroup("Running check " + stack.Name)

			stackClient := *client
			stackClient.StackName = stack.Name

			log.Info().Msgf("Waiting for stack %s to be Running (timeout: %s)...", stack.Name, p.Settings.runningTimeout)

			if err := stackClient.WaitForStackRunning(ctx, endpointID, p.Settings.runningTimeout); err != nil {
				return fmt.Errorf("running check failed: %w", err)
			}
		}
	}

	log.Info().Msgf("Configs done: %d rotated, %d checked in total; %d stack(s) restarted", len(rotated), len(plans), len(stopped))

	return nil
}

// configTeamIDs resolves the team names to IDs. Unlike stack deploys, an unknown team is an error,
// so that a typo does not leave the configs to administrators only.
func (c *Client) configTeamIDs(ctx context.Context) ([]int, error) {
	if len(c.Teams) == 0 {
		return nil, nil
	}

	var teams []portainerTeam

	if err := c.getJSON(ctx, c.ServerURL+"/api/teams", &teams); err != nil {
		return nil, fmt.Errorf("failed to list teams: %w", err)
	}

	ids := make([]int, 0, len(c.Teams))

	var unknown []string

	for _, name := range c.Teams {
		index := slices.IndexFunc(teams, func(team portainerTeam) bool { return team.Name == name })
		if index < 0 {
			unknown = append(unknown, name)

			continue
		}

		ids = append(ids, teams[index].ID)
	}

	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown team(s): %s", strings.Join(unknown, ", "))
	}

	return ids, nil
}

// planConfigs compares the config files with the configs in Swarm.
func (c *Client) planConfigs(ctx context.Context, endpointID int, entries []ConfigEntry, newData map[string][]byte) ([]*configPlan, error) {
	plans := make([]*configPlan, 0, len(entries))

	for _, entry := range entries {
		plan := &configPlan{entry: entry, newData: newData[entry.Name]}

		existing, err := c.getConfig(ctx, endpointID, entry.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to read config %q: %w", entry.Name, err)
		}

		plans = append(plans, plan)

		if existing == nil {
			log.Info().Msgf("Config %s does not exist, will create it", entry.Name)

			continue
		}

		oldData, err := base64.StdEncoding.DecodeString(existing.Spec.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode config %q: %w", entry.Name, err)
		}

		plan.existing = existing
		plan.oldData = oldData
		plan.changed = !bytes.Equal(oldData, plan.newData)

		if plan.changed {
			log.Info().Msgf("Config %s changed (%d B -> %d B), will rotate it", entry.Name, len(oldData), len(plan.newData))
		} else {
			log.Info().Msgf("Config %s is unchanged", entry.Name)
		}
	}

	return plans, nil
}

// configStacks returns the Portainer stacks whose services use the rotated configs. A rotated config
// used outside a Portainer stack on this environment cannot be released, so it is an error.
func (c *Client) configStacks(ctx context.Context, endpointID int, rotated []*configPlan) ([]portainerStack, error) {
	var services []configService

	if err := c.getJSON(ctx, c.dockerURL(endpointID, "/services"), &services); err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	nameByID := make(map[string]string, len(rotated))
	rotatedNames := make(map[string]bool, len(rotated))

	for _, plan := range rotated {
		nameByID[plan.existing.ID] = plan.entry.Name
		rotatedNames[plan.entry.Name] = true
	}

	namespaceConfigs := map[string][]string{}

	var problems []string

	for _, service := range services {
		var used []string

		for _, ref := range service.Spec.TaskTemplate.ContainerSpec.Configs {
			if name, ok := nameByID[ref.ConfigID]; ok {
				used = append(used, name)
			} else if rotatedNames[ref.ConfigName] {
				used = append(used, ref.ConfigName)
			}
		}

		if len(used) == 0 {
			continue
		}

		namespace := service.Spec.Labels[stackNamespaceLabel]
		if namespace == "" {
			problems = append(problems, fmt.Sprintf("service %s uses config(s) %s but is not part of a stack",
				service.Spec.Name, strings.Join(used, ", ")))

			continue
		}

		namespaceConfigs[namespace] = append(namespaceConfigs[namespace], used...)
	}

	var stacks []portainerStack

	if err := c.getJSON(ctx, c.ServerURL+"/api/stacks", &stacks); err != nil {
		return nil, fmt.Errorf("failed to list stacks: %w", err)
	}

	var affected []portainerStack

	for _, namespace := range slices.Sorted(maps.Keys(namespaceConfigs)) {
		index := slices.IndexFunc(stacks, func(stack portainerStack) bool {
			return stack.Name == namespace && stack.EndpointID == endpointID
		})
		if index < 0 {
			problems = append(problems, fmt.Sprintf("stack %s uses config(s) %s but is not managed by Portainer on this environment",
				namespace, strings.Join(slices.Compact(slices.Sorted(slices.Values(namespaceConfigs[namespace]))), ", ")))

			continue
		}

		affected = append(affected, stacks[index])
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("cannot rotate configs, nothing was changed:\n%s", strings.Join(problems, "\n"))
	}

	log.Info().Msgf("%d changed config(s) are used by %d stack(s)", len(rotated), len(affected))

	return affected, nil
}

// stopStacks stops the active stacks and returns the stacks to start again, also on error.
func (c *Client) stopStacks(ctx context.Context, endpointID int, stacks []portainerStack) ([]portainerStack, error) {
	var stopped []portainerStack

	for _, stack := range stacks {
		if stack.Status != stackStatusActive {
			log.Info().Msgf("Stack %s is not running, it will not be started", stack.Name)

			continue
		}

		log.Info().Msgf("Stopping stack %s", stack.Name)

		if err := c.sendJSON(ctx, http.MethodPost, c.stackActionURL(stack.ID, endpointID, "stop"), nil, nil, nil); err != nil {
			if !hasStatus(err, http.StatusConflict) {
				return stopped, fmt.Errorf("failed to stop stack %s: %w", stack.Name, err)
			}

			log.Warn().Msgf("Stack %s was already stopped", stack.Name)
		}

		stopped = append(stopped, stack)
	}

	return stopped, nil
}

// startStacks starts the stacks, trying all of them before it returns an error.
func (c *Client) startStacks(ctx context.Context, endpointID int, stacks []portainerStack) error {
	var failed []string

	for _, stack := range stacks {
		log.Info().Msgf("Starting stack %s", stack.Name)

		err := c.sendJSON(ctx, http.MethodPost, c.stackActionURL(stack.ID, endpointID, "start"), nil, nil, nil)
		if err != nil && !hasStatus(err, http.StatusConflict) {
			log.Error().Err(err).Msgf("Failed to start stack %s", stack.Name)

			failed = append(failed, stack.Name)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to start stack(s): %s", strings.Join(failed, ", "))
	}

	return nil
}

// recoverStacks starts the stopped stacks again after a failure and returns the cause.
func (c *Client) recoverStacks(ctx context.Context, endpointID int, stopped []portainerStack, cause error) error {
	if len(stopped) == 0 {
		return cause
	}

	log.Error().Msgf("Config update failed, starting %d stopped stack(s) again", len(stopped))

	if err := c.startStacks(ctx, endpointID, stopped); err != nil {
		return fmt.Errorf("%w (%w)", cause, err)
	}

	return cause
}

func (c *Client) stackActionURL(stackID, endpointID int, action string) string {
	return fmt.Sprintf("%s/api/stacks/%d/%s?endpointId=%d", c.ServerURL, stackID, action, endpointID)
}

// applyConfigPlan creates, rotates or only updates the access of a config.
func (c *Client) applyConfigPlan(ctx context.Context, endpointID int, plan *configPlan, teamIDs []int) error {
	name := plan.entry.Name

	switch {
	case plan.existing == nil:
		created, err := c.createConfig(ctx, endpointID, name, plan.newData, nil)
		if err != nil {
			return fmt.Errorf("failed to create config %q: %w", name, err)
		}

		log.Info().Msgf("Created config %s", name)

		return c.setConfigAccess(ctx, endpointID, name, created, teamIDs)
	case plan.changed:
		return c.rotateConfig(ctx, endpointID, plan, teamIDs)
	default:
		return c.setConfigAccess(ctx, endpointID, name, plan.existing, teamIDs)
	}
}

// rotateConfig deletes the config and creates it with the new content and the same labels. If the
// new config cannot be created, the old content is restored.
func (c *Client) rotateConfig(ctx context.Context, endpointID int, plan *configPlan, teamIDs []int) error {
	name := plan.entry.Name
	labels := plan.existing.Spec.Labels

	log.Info().Msgf("Deleting config %s", name)

	if err := c.deleteConfig(ctx, endpointID, plan.existing.ID, name); err != nil {
		return err
	}

	var (
		created *swarmConfig
		err     error
	)

	for attempt := 1; attempt <= configCreateAttempts; attempt++ {
		created, err = c.createConfig(ctx, endpointID, name, plan.newData, labels)
		if err == nil || attempt == configCreateAttempts {
			break
		}

		log.Warn().Err(err).Msgf("Config %s could not be created, retry %d/%d", name, attempt, configCreateAttempts-1)

		if err := sleep(ctx, c.retryDelay); err != nil {
			return err
		}
	}

	if err != nil {
		if _, restoreErr := c.createConfig(ctx, endpointID, name, plan.oldData, labels); restoreErr != nil {
			return fmt.Errorf("failed to create config %q: %w (restoring the old content also failed: %w)", name, err, restoreErr)
		}

		return fmt.Errorf("failed to create config %q: %w (old content restored)", name, err)
	}

	log.Info().Msgf("Created config %s with the new content", name)

	return c.setConfigAccess(ctx, endpointID, name, created, teamIDs)
}

// deleteConfig deletes a config, retrying while the services of the stopped stacks still use it.
func (c *Client) deleteConfig(ctx context.Context, endpointID int, configID, name string) error {
	var err error

	for attempt := 1; attempt <= configDeleteAttempts; attempt++ {
		err = c.sendJSON(ctx, http.MethodDelete, c.dockerURL(endpointID, "/configs/"+url.PathEscape(configID)), nil, nil, nil)
		if err == nil || hasStatus(err, http.StatusNotFound) {
			return nil
		}

		inUse := hasStatus(err, http.StatusBadRequest)
		if (!inUse && permanentError(err)) || attempt == configDeleteAttempts {
			break
		}

		delay := min(time.Duration(attempt)*c.retryDelay, 5*c.retryDelay)

		log.Warn().Msgf("Config %s cannot be deleted yet, retry %d/%d in %s", name, attempt, configDeleteAttempts-1, delay)

		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}

	return fmt.Errorf("failed to delete config %q: %w", name, err)
}

// setConfigAccess sets the Portainer access of a config to public, the teams or administrators only.
func (c *Client) setConfigAccess(ctx context.Context, endpointID int, name string, cfg *swarmConfig, teamIDs []int) error {
	access := resourceControlFor(c.Public, teamIDs)

	if cfg.Portainer.ResourceControl == nil || cfg.Portainer.ResourceControl.ID == 0 {
		payload := map[string]any{
			"resourceID":         cfg.ID,
			"type":               configResourceType,
			"public":             access.Public,
			"administratorsOnly": access.AdministratorsOnly,
			"teams":              access.Teams,
			"users":              []int{},
		}

		err := c.sendJSON(ctx, http.MethodPost, c.ServerURL+"/api/resource_controls", payload, nil, nil)
		if err == nil {
			logConfigAccess(name, access)

			return nil
		}

		// The access control was created in the meantime, e.g. by Portainer when the config was created.
		if !hasStatus(err, http.StatusConflict) {
			return fmt.Errorf("failed to set access for config %q: %w", name, err)
		}

		fresh, freshErr := c.getConfig(ctx, endpointID, name)
		if freshErr != nil || fresh == nil || fresh.Portainer.ResourceControl == nil {
			return fmt.Errorf("failed to set access for config %q: %w", name, err)
		}

		cfg = fresh
	}

	err := c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/api/resource_controls/%d", c.ServerURL, cfg.Portainer.ResourceControl.ID),
		access, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to set access for config %q: %w", name, err)
	}

	logConfigAccess(name, access)

	return nil
}

func logConfigAccess(name string, access ResourceControl) {
	switch {
	case access.Public:
		log.Info().Msgf("Config %s is public", name)
	case access.Restricted:
		log.Info().Msgf("Config %s is restricted to team IDs %v", name, access.Teams)
	default:
		log.Info().Msgf("Config %s is restricted to administrators", name)
	}
}

// getConfig returns the config with exactly the given name, or nil when it does not exist. The
// Docker name filter also matches configs whose name only starts with the given name.
func (c *Client) getConfig(ctx context.Context, endpointID int, name string) (*swarmConfig, error) {
	filters, err := json.Marshal(map[string][]string{"name": {name}}) //nolint:goconst // Docker filter key
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filters: %w", err)
	}

	var configs []swarmConfig

	if err := c.getJSON(ctx, c.dockerURL(endpointID, "/configs?filters="+url.QueryEscape(string(filters))), &configs); err != nil {
		return nil, err
	}

	for i := range configs {
		if configs[i].Spec.Name == name {
			return &configs[i], nil
		}
	}

	return nil, nil
}

func (c *Client) createConfig(ctx context.Context, endpointID int, name string, data []byte, labels map[string]string) (*swarmConfig, error) {
	if labels == nil {
		labels = map[string]string{}
	}

	payload := map[string]any{
		"Name":   name,
		"Labels": labels,
		"Data":   base64.StdEncoding.EncodeToString(data),
	}

	var created swarmConfig

	if err := c.sendJSON(ctx, http.MethodPost, c.dockerURL(endpointID, "/configs/create"), payload, nil, &created); err != nil {
		return nil, err
	}

	return &created, nil
}

// hasStatus reports whether err is a Portainer API error with the given status code.
func hasStatus(err error, code int) bool {
	status, ok := errors.AsType[*statusError](err)

	return ok && status.StatusCode == code
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
