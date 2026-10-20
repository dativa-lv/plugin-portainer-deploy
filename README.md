# Portainer Deploy Plugin for Woodpecker-CI

Portainer Deploy Plugin for Woodpecker-CI supports GitOps workflows by deploying Docker Swarm stack configurations from Git (the single source of truth) to your Swarm cluster via the Portainer API.

## Features

- Deploy a new stack to Portainer
- Update an existing stack
- Update services in an existing stack and pull the latest images
- Wait until all stack tasks reach the **Running** state (enabled by default)
- Run Docker Swarm job services (`replicated-job`, `global-job`), such as database migrations, and wait for them to complete. When updating services without a stack file, jobs run before the other services
- Fail the step when Portainer reports a stack deployment error
- Optionally poll a service health endpoint after the stack is running

## Configuration

Define the plugin as a step in your `.woodpecker.yaml` and provide settings under `settings:`.

## Plugin settings

| Setting | Default | Required | Description |
| --- | --- | --- | --- |
| `server-url` | _none_ | yes | Portainer base URL (e.g. `https://portainer:9000`) |
| `api-key` | _none_ | yes | Portainer API key (X-API-Key) |
| `server-environment` | _none_ | yes | Portainer environment (endpoint) name (e.g. `primary`) |
| `stack-name` | _none_ | yes | Stack name in Portainer |
| `stack-path` | _none_ | no* | Path to the stack YAML file in the workspace. Leave empty to update services in the existing stack instead of redeploying the stack file. |
| `service-name` | _empty_ | no | Optional service name to update within the stack. If empty, all services in the stack are updated. |
| `prune` | `true` | no | Prune services that are no longer referenced by the stack file |
| `teams` | _empty_ | no | Comma-separated list of Portainer team names for stack access control (e.g. `team1,team2`). If empty and `public` is not set, the stack is restricted to administrators only. |
| `public` | `false` | no | Make the stack public (accessible to all users). Cannot be combined with `teams`. |
| `running-check` | `true` | no | After deploy, wait until all stack tasks reach the `running` state |
| `running-check-timeout` | `1m` | no* | Timeout for running check (Go duration, e.g. `2m`, `30s`; `0` means no limit). Required when `running-check` is enabled. |
| `health-check` | `false` | no | After running-check succeeds, poll a health endpoint |
| `health-check-url` | _none_ | no* | Full URL to health endpoint. Required when `health-check` is enabled. |
| `health-check-timeout` | `1m` | no | Timeout for health-check polling (Go duration, e.g. `2m`, `30s`; `0` means no limit) |
| `job-timeout` | `running-check-timeout` | no | Timeout for a job service to complete (Go duration, e.g. `5m`). In stack deploys it applies to all jobs together. `0` means no limit. Only used with `running-check`. |
| `deploy-timeout` | `15m` | no | Timeout for Portainer to finish deploying the stack (Go duration, e.g. `5m`; `0` means no limit). Only applies to Portainer versions that deploy stacks in the background. |
| `log-level` | `info` | no | Logging level |
| `skip-verify` | `false` | _optional_ | Skips the SSL verification. |

\* Required only when the corresponding feature is enabled. `stack-path` is required for stack deploys, and optional for service-update mode.

## Examples

### Basic deploy (with running-check)

```yaml
steps:
  - name: deploy stack
    image: ghcr.io/dativa-lv/plugin-portainer-deploy
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
```

### Deploy + health check

```yaml
steps:
  - name: deploy stack
    image: ghcr.io/dativa-lv/plugin-portainer-deploy
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

### Update services in an existing stack

```yaml
steps:
  - name: refresh stack services
    image: ghcr.io/dativa-lv/plugin-portainer-deploy
    pull: true
    settings:
      server-url: http://portainer:9000
      api-key:
        from_secret: user-token-secret
      skip-verify: true
      server-environment: primary
      stack-name: whoami
      service-name: whoami_app
```

## Job services

Services with `deploy.mode: replicated-job` or `global-job` run to completion instead of staying running.

- **Service-update mode** (no `stack-path`): jobs selected by `service-name` (or all jobs, if it is empty) run first, with a freshly pulled image (an image pinned by digest in the stack file stays pinned). Otherwise the job runs the image tag, not a pinned digest: Portainer's Docker proxy cannot resolve digests with stored registry credentials, so each node pulls the tag when its task starts. The plugin waits until the new job iteration completes and only then updates the other services. Waiting is part of the running check: with `running-check: false` the jobs are started but not waited for, and the other services are updated right away. Failed tasks are retried by Swarm according to the job's `restart_policy` while the plugin waits. If a job does not complete within `job-timeout`, or Swarm stops retrying it (e.g. `condition: none` or `max_attempts` reached), the step fails with the latest task states and errors (e.g. `task: non-zero exit (1)`) and the other services are left unchanged.
- **Stack deploy** (`stack-path`): Swarm starts all services at once, so there is no ordering. When `running-check` is enabled, the plugin waits for the jobs this deploy started (new jobs, and jobs whose spec changed so Swarm ran them again) to complete before checking the other services. Swarm runs these jobs at the same time, so `job-timeout` applies to all of them together. Jobs the deploy did not change are not checked. If the jobs cannot be read before the deploy, the plugin logs a warning and does not check them.
- The running check does not count replicas for job services. A global job is complete when every ready, active node that got a task has a completed task. A global job that gets no task on any ready, active node (e.g. because of placement constraints) does not complete and fails at `job-timeout`.

Jobs are updated through Portainer's Docker API proxy, because Portainer's force-update endpoint cannot update jobs. The new image is pulled with the credentials of the Portainer registry whose URL matches the image's registry host (e.g. `git.example.com` for `git.example.com/org/app:tag`); images without a matching registry are pulled without credentials. If the service changes while the plugin updates a job, the update is retried. With a non-administrator API key, Portainer's resource and security restrictions for service updates apply.

## Deployment status

Portainer 2.45 and later deploy stacks in the background: the stack create or update request returns before the deployment finishes. The plugin waits until Portainer reports the result (up to `deploy-timeout`) and fails the step with Portainer's error message if the deployment failed. In that case Portainer restores the previous stack file in its editor, although Swarm may already run some of the new services. Older Portainer versions do not report a deployment status, and this check is skipped.
