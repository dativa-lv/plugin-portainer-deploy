---
name: Portainer Deploy
author: SIA Dativa
description: Portainer Deploy Plugin for Woodpecker-CI supports GitOps workflows by deploying Docker Swarm stack configurations from Git (the single source of truth) to your Swarm cluster via the Portainer API.
tags: [deploy, publish, docker]
containerImage:  ghcr.io/dativa-lv/plugin-portainer-deploy
containerImageUrl: https://github.com/dativa-lv/plugin-portainer-deploy/pkgs/container/plugin-portainer-deploy
url: https://github.com/dativa-lv/plugin-portainer-deploy
---

Portainer Deploy Plugin for Woodpecker-CI supports GitOps workflows by deploying Docker Swarm stack configurations from Git (the single source of truth) to your Swarm cluster via the Portainer API.

## Features

- Deploy a new stack to Portainer
- Update an existing stack
- Wait until all stack tasks reach the **Running** state (enabled by default)
- Optionally poll a service health endpoint after the stack is running

## Example

```yaml
steps:
  - name: deploy stack
    image: woodpeckerci/portainer-deploy-plugin
    pull: true
    settings:
      server-url: http://portainer:9000
      api-key:
        from_secret: user-token-secret
      skip-verify: true
      server-environment: primary
      stack-name: whoami
      stack-path: whoami.yml
      teams: team1,team2
      running-check-timeout: 2m # Running check bumped to 2 minute timeout
      health-check: true # Health check enabled
      health-check-url: https://example.com/whoami/healthz # Health check URL
      health-check-timeout: 2m # Health check bumped to 2 minute timeout 
      log-level: debug # Logging set to debug level
```

## Settings

| Setting | Default | Required | Description |
| --- | --- | --- | --- |
| `server-url` | _none_ | yes | Portainer base URL (e.g. `https://portainer:9000`) |
| `api-key` | _none_ | yes | Portainer API key (X-API-Key) |
| `server-environment` | _none_ | yes | Portainer environment (endpoint) name (e.g. `primary`) |
| `stack-name` | _none_ | yes | Stack name in Portainer |
| `stack-path` | _none_ | yes | Path to the stack YAML file in the workspace |
| `prune` | `true` | no | Prune services that are no longer referenced by the stack file |
| `teams` | _empty_ | no | Comma-separated list of Portainer team names for stack access control (e.g. `team1,team2`). If empty, the stack is restricted to administrators only. |
| `running-check` | `true` | no | After deploy, wait until all stack tasks reach the `running` state |
| `running-check-timeout` | `1m` | no* | Timeout for running check (Go duration, e.g. `2m`, `30s`). Required when `running-check` is enabled. |
| `health-check` | `false` | no | After running-check succeeds, poll a health endpoint |
| `health-check-url` | _none_ | no* | Full URL to health endpoint. Required when `health-check` is enabled. |
| `health-check-timeout` | `1m` | no | Timeout for health-check polling (Go duration, e.g. `2m`, `30s`) |
| `log-level` | `info` | no | Logging level |
| `skip-verify` | `false` | _optional_ | Skips the SSL verification. |

\* Required only when the corresponding feature is enabled.
