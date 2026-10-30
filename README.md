# k8s-job-cleaner-go

Deletes finished Kubernetes Jobs and standalone terminated Pods once they are older than a retention TTL. Run it as a CronJob.

Inspired by [dtan4/k8s-job-cleaner](https://github.com/dtan4/k8s-job-cleaner).

Kubernetes can already do this per Job with `spec.ttlSecondsAfterFinished`. This tool is for Jobs you don't control (Helm charts, CI runners) and for keeping failed Jobs longer than succeeded ones.

## Behavior

- Jobs are deleted with background propagation, so their Pods go with them.
- Pods owned by a Job are left to that cascade. Only standalone Succeeded/Failed Pods are deleted directly.
- Jobs that set `spec.ttlSecondsAfterFinished` are skipped (override with `--ignore-native-ttl`).
- `kube-system`, `kube-public` and `kube-node-lease` are skipped.
- Uses the in-cluster config, or falls back to your kubeconfig.
- Logs every deletion and a summary per run. `--verbose` also logs each skip and its reason.

## Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--namespace` | all | Namespace to clean |
| `--exclude-namespaces` | `kube-system,kube-public,kube-node-lease` | Namespaces to skip |
| `--succeeded-ttl` | `0s` | Min age after completion before deleting |
| `--failed-ttl` | `0s` | Min age after failure before deleting |
| `--max-count` | `100` | Max jobs and max pods deleted per run |
| `--dry-run` | `false` | Log deletions without performing them |
| `--ignore-native-ttl` | `false` | Also clean Jobs that set their own TTL |
| `--verbose` | `false` | Log skipped resources |
| `--kubeconfig` | | Kubeconfig path (outside the cluster) |

## Deploy

```bash
kubectl apply -f cronjob.yaml        # RBAC + CronJob
```

For a local cluster (Docker Desktop, kind, minikube), build the image and use `cronjob.local.yaml`, which sets `imagePullPolicy: Never`:

```bash
docker build -t knpavankumar/k8s-job-cleaner-go:latest .
kubectl apply -f cronjob.local.yaml
```

## Development

```bash
go run . --dry-run --verbose --succeeded-ttl=1h --failed-ttl=24h
go test ./...
```

Images are published to Docker Hub on pushes to `main`/`master` (`latest`, `sha-<short>`) and on `vX.Y.Z` tags (`X.Y.Z`, `X.Y`, `X`).
