// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"fmt"
	"maps"
	"slices"

	"github.com/rs/zerolog/log"
)

// Stack rules apply to every stack and cannot be turned off:
//   - every service must set deploy.resources.limits.memory and deploy.resources.limits.cpus;
//   - a replicated service with a single replica gets a warning (it is down while it restarts).

// lintStackFile applies the stack rules to the stack file about to be deployed.
func lintStackFile(path string) error {
	stacks, err := loadStacks(path)
	if err != nil {
		return err
	}

	return lintStacks(stacks)
}

// lintStacks applies the stack rules and returns an error if any stack breaks them.
func lintStacks(stacks []loadedStack) error {
	var violations []string

	for _, loaded := range stacks {
		for _, serviceName := range slices.Sorted(maps.Keys(loaded.stack.Services)) {
			deploy := loaded.stack.Services[serviceName].Deploy

			for _, limit := range missingLimits(deploy) {
				violations = append(violations,
					fmt.Sprintf("%s: service %s has no deploy.resources.limits.%s", loaded.file, serviceName, limit))
			}

			if runsSingleReplica(deploy) {
				log.Warn().Msgf("%s: service %s runs a single replica", loaded.file, serviceName)
			}
		}
	}

	for _, violation := range violations {
		log.Error().Msg(violation)
	}

	if len(violations) > 0 {
		return fmt.Errorf("%d stack rule violation(s): every service needs CPU and memory limits", len(violations))
	}

	return nil
}

// missingLimits returns the names of the resource limits the service does not set.
func missingLimits(deploy serviceDeploy) []string {
	var missing []string

	if deploy.Resources.Limits.CPUs == nil {
		missing = append(missing, "cpus")
	}

	if deploy.Resources.Limits.Memory == nil {
		missing = append(missing, "memory")
	}

	return missing
}

// runsSingleReplica reports whether a replicated service runs exactly one replica.
// Global and job services are not replicated, and a ${VARIABLE} count is unknown.
func runsSingleReplica(deploy serviceDeploy) bool {
	if deploy.Mode != "" && deploy.Mode != "replicated" {
		return false
	}

	switch replicas := deploy.Replicas.(type) {
	case nil:
		return true // Swarm runs one replica when none is set
	case int:
		return replicas == 1
	default:
		return false
	}
}
