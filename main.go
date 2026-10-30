package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type config struct {
	namespace         string
	excludeNamespaces map[string]bool
	kubeconfig        string
	dryRun            bool
	ignoreNativeTTL   bool
	maxCount          int
	succeededTTL      time.Duration
	failedTTL         time.Duration
}

// decision is the outcome of evaluating one Job or Pod.
type decision struct {
	deletable bool
	reason    string
	age, ttl  time.Duration
}

func parseNamespaceSet(csv string) map[string]bool {
	set := make(map[string]bool)
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	return set
}

// expired compares the time since finishedAt against ttl.
func expired(finishedAt time.Time, ttl time.Duration, now time.Time) decision {
	age := max(now.Sub(finishedAt), 0)
	if age < ttl {
		return decision{reason: "within_retention_ttl", age: age, ttl: ttl}
	}
	return decision{deletable: true, reason: "retention_ttl_expired", age: age, ttl: ttl}
}

// finishedJobState reports whether the job is finished, whether it succeeded,
// and when it finished.
func finishedJobState(job *batchv1.Job) (finished, succeeded bool, at time.Time) {
	if job.Status.Active > 0 {
		return false, false, time.Time{}
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue || (c.Type != batchv1.JobComplete && c.Type != batchv1.JobFailed) {
			continue
		}
		at = c.LastTransitionTime.Time
		if at.IsZero() && job.Status.CompletionTime != nil {
			at = job.Status.CompletionTime.Time
		}
		if at.IsZero() {
			at = job.CreationTimestamp.Time
		}
		return true, c.Type == batchv1.JobComplete, at
	}
	return false, false, time.Time{}
}

func evaluateJob(job *batchv1.Job, cfg config, now time.Time) decision {
	switch {
	case cfg.excludeNamespaces[job.Namespace]:
		return decision{reason: "excluded_namespace"}
	case !cfg.ignoreNativeTTL && job.Spec.TTLSecondsAfterFinished != nil:
		return decision{reason: "managed_by_native_ttl"}
	}
	finished, succeeded, at := finishedJobState(job)
	if !finished {
		return decision{reason: "active_or_incomplete"}
	}
	ttl := cfg.failedTTL
	if succeeded {
		ttl = cfg.succeededTTL
	}
	return expired(at, ttl, now)
}

func isOwnedByJob(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" && strings.HasPrefix(ref.APIVersion, "batch/") {
			return true
		}
	}
	return false
}

// podFinishedAt returns the latest container termination time, falling back to creation time.
func podFinishedAt(pod *corev1.Pod) time.Time {
	latest := pod.CreationTimestamp.Time
	var found bool
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			if t := cs.State.Terminated; t != nil && (!found || t.FinishedAt.Time.After(latest)) {
				latest, found = t.FinishedAt.Time, true
			}
		}
	}
	return latest
}

func evaluatePod(pod *corev1.Pod, cfg config, now time.Time) decision {
	if cfg.excludeNamespaces[pod.Namespace] {
		return decision{reason: "excluded_namespace"}
	}
	if isOwnedByJob(pod) {
		return decision{reason: "owned_by_job"}
	}
	var ttl time.Duration
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		ttl = cfg.succeededTTL
	case corev1.PodFailed:
		ttl = cfg.failedTTL
	default:
		return decision{reason: "not_terminated"}
	}
	return expired(podFinishedAt(pod), ttl, now)
}

func buildClient(kubeconfig string) (kubernetes.Interface, string, error) {
	restCfg, err := rest.InClusterConfig()
	mode := "in-cluster"
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = kubeconfig
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("no in-cluster or kubeconfig configuration: %w", err)
		}
		mode = "kubeconfig"
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	return cs, mode, err
}

// sweep evaluates n items, deletes the deletable ones (up to cfg.maxCount) and
// logs every action plus a summary. It returns the number deleted.
func sweep(kind string, n int, cfg config,
	eval func(i int) (namespace, name string, d decision),
	del func(i int) error) int {

	var deleted, failed int
	skipped := map[string]int{}
	for i := 0; i < n; i++ {
		ns, name, d := eval(i)
		attrs := []any{"namespace", ns, "name", name, "reason", d.reason, "age", d.age.Round(time.Second), "ttl", d.ttl}
		if !d.deletable {
			skipped[d.reason]++
			slog.Debug("skipping "+kind, attrs...)
			continue
		}
		if deleted >= cfg.maxCount {
			skipped["max_count_reached"]++
			slog.Debug("skipping "+kind, "namespace", ns, "name", name, "reason", "max_count_reached")
			continue
		}
		slog.Info("deleting "+kind, append(attrs, "dryRun", cfg.dryRun)...)
		if !cfg.dryRun {
			if err := del(i); apierrors.IsNotFound(err) {
				slog.Warn(kind+" already gone", "namespace", ns, "name", name)
				continue
			} else if err != nil {
				failed++
				slog.Error("failed to delete "+kind, "namespace", ns, "name", name, "error", err)
				continue
			}
		}
		deleted++
	}
	slog.Info(kind+" cleanup summary", "scanned", n, "deleted", deleted, "failed", failed, "skipped", skipped, "dryRun", cfg.dryRun)
	return deleted
}

func run(ctx context.Context, cs kubernetes.Interface, cfg config) error {
	start := time.Now()

	jobs, err := cs.BatchV1().Jobs(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	background := metav1.DeletePropagationBackground
	jobsDeleted := sweep("job", len(jobs.Items), cfg,
		func(i int) (string, string, decision) {
			j := &jobs.Items[i]
			return j.Namespace, j.Name, evaluateJob(j, cfg, start)
		},
		func(i int) error {
			j := &jobs.Items[i]
			return cs.BatchV1().Jobs(j.Namespace).Delete(ctx, j.Name, metav1.DeleteOptions{PropagationPolicy: &background})
		})

	pods, err := cs.CoreV1().Pods(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	podsDeleted := sweep("pod", len(pods.Items), cfg,
		func(i int) (string, string, decision) {
			p := &pods.Items[i]
			return p.Namespace, p.Name, evaluatePod(p, cfg, start)
		},
		func(i int) error {
			p := &pods.Items[i]
			return cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
		})

	slog.Info("cleanup run completed", "duration", time.Since(start).Round(time.Millisecond),
		"jobsDeleted", jobsDeleted, "podsDeleted", podsDeleted, "dryRun", cfg.dryRun)
	return nil
}

func main() {
	var (
		cfg        config
		excludeCSV string
		verbose    bool
	)
	flag.StringVar(&cfg.namespace, "namespace", metav1.NamespaceAll, "namespace to clean (default: all namespaces)")
	flag.StringVar(&excludeCSV, "exclude-namespaces", "kube-system,kube-public,kube-node-lease", "comma-separated namespaces to skip")
	flag.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to kubeconfig (used when not running in-cluster)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "log what would be deleted without deleting")
	flag.BoolVar(&cfg.ignoreNativeTTL, "ignore-native-ttl", false, "also clean jobs that set spec.ttlSecondsAfterFinished")
	flag.IntVar(&cfg.maxCount, "max-count", 100, "maximum number of jobs and of pods to delete per run")
	flag.DurationVar(&cfg.succeededTTL, "succeeded-ttl", 0, "minimum age after completion before deleting succeeded jobs/pods")
	flag.DurationVar(&cfg.failedTTL, "failed-ttl", 0, "minimum age after failure before deleting failed jobs/pods")
	flag.BoolVar(&verbose, "verbose", false, "also log every skipped resource and why")
	flag.Parse()

	cfg.excludeNamespaces = parseNamespaceSet(excludeCSV)
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cs, mode, err := buildClient(cfg.kubeconfig)
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}
	slog.Info("starting", "auth", mode, "namespace", cfg.namespace, "exclude", excludeCSV, "dryRun", cfg.dryRun,
		"maxCount", cfg.maxCount, "succeededTTL", cfg.succeededTTL, "failedTTL", cfg.failedTTL)

	if err := run(ctx, cs, cfg); err != nil {
		slog.Error("cleanup failed", "error", err)
		os.Exit(1)
	}
}
