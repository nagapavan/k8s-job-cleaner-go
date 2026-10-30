# k8s-job-cleaner-go

A lightweight Kubernetes utility to clean up finished (`Complete` or `Failed`) Kubernetes `Jobs` and standalone terminated `Pods` after configurable retention TTLs.

Inspired by [dtan4/k8s-job-cleaner](https://github.com/dtan4/k8s-job-cleaner).

## Why Use This?

While modern Kubernetes provides `spec.ttlSecondsAfterFinished` on Jobs and history limits on CronJobs, clusters often accumulate completed/failed Jobs and orphaned Pods from:
- Third-party Helm charts, CI runners, or legacy manifests that omit `ttlSecondsAfterFinished`.
- Workloads needing separate retention windows for **Succeeded** jobs (e.g., clean after `1h`) versus **Failed** jobs (e.g., retain for `24h` for debugging).
- Standalone/orphaned terminated Pods not managed by a parent Job.

## Features

- **Differentiated Retention TTLs**: Separate `--succeeded-ttl` and `--failed-ttl` durations.
- **Safe Defaults**:
  - Automatically skips `kube-system`, `kube-public`, and `kube-node-lease` namespaces (`--exclude-namespaces`).
  - Automatically respects Jobs that already define native `spec.ttlSecondsAfterFinished` (unless `--ignore-native-ttl` is set).
  - Skips Pods owned by a `Job` during Pod cleanup so `DeletePropagationBackground` handles them cleanly without race conditions.
- **Transparent Structured Logging**: Built-in `log/slog` logging (`text` or `json`) with per-resource audit reasons (`retention_ttl_expired`, `within_retention_ttl`, `managed_by_native_ttl`, `excluded_namespace`, `owned_by_job`) and end-of-run summary statistics.
- **Dual Auth Mode**: Automatically uses in-cluster service account config when running inside Kubernetes, falling back to local `KUBECONFIG` / `~/.kube/config` for local runs.
- **Hardened Container Image**: Multi-arch (`linux/amd64`, `linux/arm64`) static binary running as `nonroot` on `gcr.io/distroless/static-debian12:nonroot`.

## CLI Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--namespace` | `""` (all) | Namespace to clean (default: all namespaces) |
| `--exclude-namespaces` | `kube-system,kube-public,kube-node-lease` | Comma-separated namespaces to exclude from cleanup |
| `--succeeded-ttl` | `0s` | Minimum age after completion before deleting succeeded jobs/pods (e.g. `1h`, `30m`) |
| `--failed-ttl` | `0s` | Minimum age after failure before deleting failed jobs/pods (e.g. `24h`) |
| `--max-count` | `100` | Maximum number of jobs and of pods to delete per run |
| `--dry-run` | `false` | Log what would be deleted without performing deletions |
| `--ignore-native-ttl` | `false` | Also clean jobs that have `spec.ttlSecondsAfterFinished` set |
| `--log-level` | `info` | Log verbosity (`debug`, `info`, `warn`, `error`). Use `debug` to log every skipped resource and reason |
| `--log-format` | `text` | Log output format (`text` or `json`) |
| `--kubeconfig` | `""` | Explicit path to kubeconfig file (when running outside cluster) |

## Deployment

### Standard Kubernetes Cluster
Includes `ServiceAccount`, `ClusterRole`, `ClusterRoleBinding`, and `batch/v1` `CronJob`:
```bash
kubectl apply -f cronjob.yaml
```
See [cronjob.yaml](cronjob.yaml).

### Local Kubernetes Cluster (Docker Desktop / kind / minikube)
Uses local image with `imagePullPolicy: Never`:
```bash
docker build -t knpavankumar/k8s-job-cleaner-go:latest .
kubectl apply -f cronjob.local.yaml
```
See [cronjob.local.yaml](cronjob.local.yaml).

## Local Development & Testing

Run locally against your current kubeconfig in dry-run debug mode:
```bash
go run . --dry-run --log-level=debug --succeeded-ttl=1h --failed-ttl=24h
```

Run unit tests:
```bash
go test -race -v ./...
```
