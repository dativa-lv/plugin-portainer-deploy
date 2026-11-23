// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"fmt"
	"maps"
	"slices"

	"github.com/rs/zerolog/log"
)

// lintStacks applies the stack rules and returns an error if any stack breaks them.
func lintStacks(stacks []loadedStack) error {
	violationCount := 0

	for _, loaded := range stacks {
		for _, serviceName := range slices.Sorted(maps.Keys(loaded.stack.Services)) {
			service := loaded.stack.Services[serviceName]
			if service.Image == "" {
				continue
			}

			violationCount += lintService(loaded.file, serviceName, service.Deploy)
		}
	}

	if violationCount == 0 {
		return nil
	}

	return fmt.Errorf("%d stack rule violation(s): every service needs CPU and memory limits", violationCount)
}

// lintService logs the service's violations and warnings and returns the number of violations.
func lintService(file, serviceName string, deploy serviceDeploy) int {
	if runsSingleReplica(deploy) {
		log.Warn().Msgf("%s: service %s runs a single replica", file, serviceName)
	}

	missing := missingLimits(deploy)
	for _, limit := range missing {
		log.Error().Msgf("%s: service %s has no deploy.resources.limits.%s", file, serviceName, limit)
	}

	return len(missing)
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
func runsSingleReplica(deploy serviceDeploy) bool {
	if deploy.Mode != "" && deploy.Mode != "replicated" {
		return false
	}

	switch replicas := deploy.Replicas.(type) {
	case nil:
		return true
	case int:
		return replicas == 1
	default:
		return false
	}
}
