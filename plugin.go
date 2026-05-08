// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"os"

	"codeberg.org/woodpecker-plugins/go-plugin"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

// Settings for the plugin.
type Settings struct {
	APIKey             string
	ServerURL          string
	ServerEnvironment  string
	StackName          string
	StackPath          string
	ServiceName        string
	Prune              bool
	Teams              string
	RunningCheck       bool
	RunningTimeout     string
	HealthCheck        bool
	HealthCheckURL     string
	HealthCheckTimeout string
}

// Plugin implements provide the plugin.
type Plugin struct {
	*plugin.Plugin

	Settings *Settings
}

// New creates and initializes a new plugin.
func New(version string) *Plugin {
	if envFile, set := os.LookupEnv("PLUGIN_ENV_FILE"); set {
		err := godotenv.Overload(envFile)
		if err != nil {
			log.Error().Err(err).Msg("couldn't load env file")
		}
	}

	p := &Plugin{
		Settings: &Settings{},
	}

	p.Plugin = plugin.New(plugin.Options{
		Name:        "portainer-deploy-plugin",
		Description: "Plugin for service deploy via Portainer API",
		Version:     version,
		Flags:       p.Flags(),
		Execute:     p.Execute,
	})

	return p
}

// Flags returns a slice of CLI flags for the plugin.
func (p *Plugin) Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:        "server-url",
			Sources:     cli.EnvVars("PLUGIN_SERVER_URL"),
			Usage:       "Portainer base url",
			Destination: &p.Settings.ServerURL,
		},
		&cli.StringFlag{
			Name:        "api-key",
			Usage:       "api key to access api",
			Sources:     cli.EnvVars("PLUGIN_API_KEY"),
			Destination: &p.Settings.APIKey,
		},
		&cli.StringFlag{
			Name:        "server-environment",
			Sources:     cli.EnvVars("PLUGIN_SERVER_ENVIRONMENT"),
			Usage:       "Portainer environment name",
			Destination: &p.Settings.ServerEnvironment,
		},
		&cli.StringFlag{
			Name:        "stack-name",
			Sources:     cli.EnvVars("PLUGIN_STACK_NAME"),
			Usage:       "Stack name",
			Destination: &p.Settings.StackName,
		},
		&cli.StringFlag{
			Name:        "stack-path",
			Sources:     cli.EnvVars("PLUGIN_STACK_PATH"),
			Usage:       "Stack YAML filename (optional when updating stack services)",
			Destination: &p.Settings.StackPath,
		},
		&cli.StringFlag{
			Name:        "service-name",
			Sources:     cli.EnvVars("PLUGIN_SERVICE_NAME"),
			Usage:       "Optional service name to update within the stack",
			Destination: &p.Settings.ServiceName,
		},
		&cli.BoolFlag{
			Name:        "prune",
			Sources:     cli.EnvVars("PLUGIN_PRUNE"),
			Usage:       "Prune stack",
			Value:       true,
			Destination: &p.Settings.Prune,
		},
		&cli.StringFlag{
			Name:        "teams",
			Sources:     cli.EnvVars("PLUGIN_TEAMS"),
			Usage:       "Comma separated list of teams",
			Destination: &p.Settings.Teams,
		},
		&cli.BoolFlag{
			Name:        "running-check",
			Sources:     cli.EnvVars("PLUGIN_RUNNING_CHECK"),
			Usage:       "Wait for all stack tasks to reach running state after deploy",
			Value:       true,
			Destination: &p.Settings.RunningCheck,
		},
		&cli.StringFlag{
			Name:        "running-check-timeout",
			Sources:     cli.EnvVars("PLUGIN_RUNNING_CHECK_TIMEOUT"),
			Usage:       "Running check timeout in Go duration format (e.g. 1m, 2m)",
			Value:       "1m",
			Destination: &p.Settings.RunningTimeout,
		},
		&cli.BoolFlag{
			Name:        "health-check",
			Sources:     cli.EnvVars("PLUGIN_HEALTH_CHECK"),
			Usage:       "Poll a health endpoint after deploy",
			Value:       false,
			Destination: &p.Settings.HealthCheck,
		},
		&cli.StringFlag{
			Name:        "health-check-url",
			Sources:     cli.EnvVars("PLUGIN_HEALTH_CHECK_URL", "PLUGIN_HELTH_CHECK_URL"),
			Usage:       "Health-check endpoint URL",
			Destination: &p.Settings.HealthCheckURL,
		},
		&cli.StringFlag{
			Name:        "health-check-timeout",
			Sources:     cli.EnvVars("PLUGIN_HEALTH_CHECK_TIMEOUT"),
			Usage:       "Timeout for health check polling (e.g. 1m, 30s)",
			Value:       "1m",
			Destination: &p.Settings.HealthCheckTimeout,
		},
	}
}
