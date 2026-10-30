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

// config holds the runtime options of the cleaner.
type config struct {
	namespace         string
	excludeNamespaces map[string]bool
	kubeconfig        string
	dryRun            bool
	ignoreNativeTTL   bool
	maxCount          int
	succeededTTL      time.Duration
	failedTTL         time.Duration
	logLevel          string
	logFormat         string
}

// decision describes whether a resource should be deleted and why.
type decision struct {
	deletable bool
	reason    string
	state     string
	age       time.Duration
	ttl       time.Duration
}

// stats tracks evaluation outcomes for summary audit logging.
type stats struct {
	total             int
	deleted           int
	failedDeletes     int
	skippedNamespace  int
	skippedActive     int
	skippedNativeTTL  int
	skippedJobOwned   int
	skippedRetention  int
	skippedMaxReached int
}

func parseNamespaceSet(csv string) map[string]bool {
	set := make(map[string]bool)
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			set[s] = true
		}
	}
	return set
}

func setupLogger(levelStr, format string) error {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("invalid log-level %q (expected debug, info, warn, error)", levelStr)
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	case "text", "":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		return fmt.Errorf("invalid log-format %q (expected text or json)", format)
	}
	slog.SetDefault(slog.New(handler))
	return nil
}

// finishedJobState reports whether the job is finished, whether it succeeded,
// and when it finished.
func finishedJobState(job *batchv1.Job) (finished, succeeded bool, at time.Time) {
	if job.Status.Active > 0 {
		return false, false, time.Time{}
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			t := c.LastTransitionTime.Time
			if t.IsZero() && job.Status.CompletionTime != nil {
				t = job.Status.CompletionTime.Time
			}
			if t.IsZero() {
				t = job.CreationTimestamp.Time
			}
			return true, true, t
		case batchv1.JobFailed:
			t := c.LastTransitionTime.Time
			if t.IsZero() {
				t = job.CreationTimestamp.Time
			}
			return true, false, t
		}
	}
	if job.Status.Succeeded > 0 {
		t := job.CreationTimestamp.Time
		if job.Status.CompletionTime != nil && !job.Status.CompletionTime.Time.IsZero() {
			t = job.Status.CompletionTime.Time
		}
		return true, true, t
	}
	return false, false, time.Time{}
}

// evaluateJob inspects a Job and returns a transparent decision on whether to delete it.
func evaluateJob(job *batchv1.Job, cfg config, now time.Time) decision {
	if cfg.excludeNamespaces[job.Namespace] {
		return decision{deletable: false, reason: "excluded_namespace", state: "Ignored"}
	}
	if !cfg.ignoreNativeTTL && job.Spec.TTLSecondsAfterFinished != nil {
		return decision{deletable: false, reason: "managed_by_native_ttl", state: "NativeTTL"}
	}
	finished, succeeded, at := finishedJobState(job)
	if !finished {
		return decision{deletable: false, reason: "active_or_incomplete", state: "Active"}
	}

	state := "Failed"
	ttl := cfg.failedTTL
	if succeeded {
		state = "Succeeded"
		ttl = cfg.succeededTTL
	}

	age := now.Sub(at)
	if age < 0 {
		age = 0
	}
	if age < ttl {
		return decision{deletable: false, reason: "within_retention_ttl", state: state, age: age, ttl: ttl}
	}
	return decision{deletable: true, reason: "retention_ttl_expired", state: state, age: age, ttl: ttl}
}

// isDeletableJob returns true when the job finished and exceeded its retention TTL.
func isDeletableJob(job *batchv1.Job, cfg config, now time.Time) bool {
	return evaluateJob(job, cfg, now).deletable
}

func isOwnedByJob(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" && strings.HasPrefix(ref.APIVersion, "batch/") {
			return true
		}
	}
	return false
}

// podFinishedAt returns the best-known time a terminated pod finished.
func podFinishedAt(pod *corev1.Pod) time.Time {
	var latest time.Time
	for _, cs := range pod.Status.InitContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.FinishedAt.Time.After(latest) {
			latest = t.FinishedAt.Time
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.FinishedAt.Time.After(latest) {
			latest = t.FinishedAt.Time
		}
	}
	if latest.IsZero() {
		latest = pod.CreationTimestamp.Time
	}
	return latest
}

// evaluatePod inspects a Pod and returns a transparent decision on whether to delete it.
func evaluatePod(pod *corev1.Pod, cfg config, now time.Time) decision {
	if cfg.excludeNamespaces[pod.Namespace] {
		return decision{deletable: false, reason: "excluded_namespace", state: string(pod.Status.Phase)}
	}
	if isOwnedByJob(pod) {
		return decision{deletable: false, reason: "owned_by_job", state: string(pod.Status.Phase)}
	}

	var ttl time.Duration
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		ttl = cfg.succeededTTL
	case corev1.PodFailed:
		ttl = cfg.failedTTL
	default:
		return decision{deletable: false, reason: "active_or_non_terminal", state: string(pod.Status.Phase)}
	}

	age := now.Sub(podFinishedAt(pod))
	if age < 0 {
		age = 0
	}
	if age < ttl {
		return decision{deletable: false, reason: "within_retention_ttl", state: string(pod.Status.Phase), age: age, ttl: ttl}
	}
	return decision{deletable: true, reason: "retention_ttl_expired", state: string(pod.Status.Phase), age: age, ttl: ttl}
}

// isDeletablePod returns true for terminated standalone pods that exceeded their retention TTL.
func isDeletablePod(pod *corev1.Pod, cfg config, now time.Time) bool {
	return evaluatePod(pod, cfg, now).deletable
}

func buildClient(kubeconfig string) (kubernetes.Interface, string, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		cs, err := kubernetes.NewForConfig(cfg)
		return cs, "in-cluster", err
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("no in-cluster or kubeconfig configuration: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	return cs, "kubeconfig", err
}

func cleanJobs(ctx context.Context, cs kubernetes.Interface, cfg config, now time.Time) (stats, error) {
	var st stats
	jobs, err := cs.BatchV1().Jobs(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return st, fmt.Errorf("listing jobs: %w", err)
	}
	st.total = len(jobs.Items)
	slog.Info("listed jobs for evaluation", "total", st.total, "namespace", cfg.namespace)

	policy := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		job := &jobs.Items[i]
		dec := evaluateJob(job, cfg, now)
		if !dec.deletable {
			switch dec.reason {
			case "excluded_namespace":
				st.skippedNamespace++
			case "managed_by_native_ttl":
				st.skippedNativeTTL++
			case "active_or_incomplete":
				st.skippedActive++
			case "within_retention_ttl":
				st.skippedRetention++
			}
			slog.Debug("skipping job",
				"namespace", job.Namespace,
				"name", job.Name,
				"state", dec.state,
				"reason", dec.reason,
				"age", dec.age.Round(time.Second),
				"ttl", dec.ttl,
			)
			continue
		}
		if st.deleted >= cfg.maxCount {
			st.skippedMaxReached++
			slog.Debug("skipping job due to max-count limit",
				"namespace", job.Namespace,
				"name", job.Name,
				"maxCount", cfg.maxCount,
			)
			continue
		}

		slog.Info("deleting job",
			"namespace", job.Namespace,
			"name", job.Name,
			"state", dec.state,
			"reason", dec.reason,
			"age", dec.age.Round(time.Second),
			"ttl", dec.ttl,
			"dryRun", cfg.dryRun,
		)
		if cfg.dryRun {
			st.deleted++
			continue
		}
		err := cs.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &policy})
		if err != nil {
			if apierrors.IsNotFound(err) {
				slog.Warn("job already deleted concurrently", "namespace", job.Namespace, "name", job.Name)
				continue
			}
			st.failedDeletes++
			slog.Error("failed to delete job", "namespace", job.Namespace, "name", job.Name, "error", err)
			continue
		}
		st.deleted++
	}

	slog.Info("job cleanup summary",
		"totalScanned", st.total,
		"deleted", st.deleted,
		"failedDeletes", st.failedDeletes,
		"skippedActive", st.skippedActive,
		"skippedWithinTTL", st.skippedRetention,
		"skippedNativeTTL", st.skippedNativeTTL,
		"skippedExcludedNamespace", st.skippedNamespace,
		"skippedMaxCount", st.skippedMaxReached,
		"dryRun", cfg.dryRun,
	)
	return st, nil
}

func cleanPods(ctx context.Context, cs kubernetes.Interface, cfg config, now time.Time) (stats, error) {
	var st stats
	pods, err := cs.CoreV1().Pods(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return st, fmt.Errorf("listing pods: %w", err)
	}
	st.total = len(pods.Items)
	slog.Info("listed pods for evaluation", "total", st.total, "namespace", cfg.namespace)

	for i := range pods.Items {
		pod := &pods.Items[i]
		dec := evaluatePod(pod, cfg, now)
		if !dec.deletable {
			switch dec.reason {
			case "excluded_namespace":
				st.skippedNamespace++
			case "owned_by_job":
				st.skippedJobOwned++
			case "active_or_non_terminal":
				st.skippedActive++
			case "within_retention_ttl":
				st.skippedRetention++
			}
			slog.Debug("skipping pod",
				"namespace", pod.Namespace,
				"name", pod.Name,
				"phase", dec.state,
				"reason", dec.reason,
				"age", dec.age.Round(time.Second),
				"ttl", dec.ttl,
			)
			continue
		}
		if st.deleted >= cfg.maxCount {
			st.skippedMaxReached++
			slog.Debug("skipping pod due to max-count limit",
				"namespace", pod.Namespace,
				"name", pod.Name,
				"maxCount", cfg.maxCount,
			)
			continue
		}

		slog.Info("deleting pod",
			"namespace", pod.Namespace,
			"name", pod.Name,
			"phase", dec.state,
			"reason", dec.reason,
			"age", dec.age.Round(time.Second),
			"ttl", dec.ttl,
			"dryRun", cfg.dryRun,
		)
		if cfg.dryRun {
			st.deleted++
			continue
		}
		if err := cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				slog.Warn("pod already deleted concurrently", "namespace", pod.Namespace, "name", pod.Name)
				continue
			}
			st.failedDeletes++
			slog.Error("failed to delete pod", "namespace", pod.Namespace, "name", pod.Name, "error", err)
			continue
		}
		st.deleted++
	}

	slog.Info("pod cleanup summary",
		"totalScanned", st.total,
		"deleted", st.deleted,
		"failedDeletes", st.failedDeletes,
		"skippedActive", st.skippedActive,
		"skippedWithinTTL", st.skippedRetention,
		"skippedJobOwned", st.skippedJobOwned,
		"skippedExcludedNamespace", st.skippedNamespace,
		"skippedMaxCount", st.skippedMaxReached,
		"dryRun", cfg.dryRun,
	)
	return st, nil
}

func run(ctx context.Context, cs kubernetes.Interface, cfg config) error {
	start := time.Now()
	jobStats, err := cleanJobs(ctx, cs, cfg, start)
	if err != nil {
		return err
	}

	podStats, err := cleanPods(ctx, cs, cfg, start)
	if err != nil {
		return err
	}

	slog.Info("cleanup run completed",
		"duration", time.Since(start).Round(time.Millisecond),
		"jobsDeleted", jobStats.deleted,
		"podsDeleted", podStats.deleted,
		"dryRun", cfg.dryRun,
	)
	return nil
}

func main() {
	var (
		cfg        config
		excludeCSV string
	)
	flag.StringVar(&cfg.namespace, "namespace", metav1.NamespaceAll, "namespace to clean (default: all namespaces)")
	flag.StringVar(&excludeCSV, "exclude-namespaces", "kube-system,kube-public,kube-node-lease", "comma-separated namespaces to exclude from cleanup")
	flag.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to kubeconfig (used when not running in-cluster)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "log what would be deleted without deleting")
	flag.BoolVar(&cfg.ignoreNativeTTL, "ignore-native-ttl", false, "also clean jobs that have spec.ttlSecondsAfterFinished set")
	flag.IntVar(&cfg.maxCount, "max-count", 100, "maximum number of jobs and of pods to delete per run")
	flag.DurationVar(&cfg.succeededTTL, "succeeded-ttl", 0, "minimum age after completion before deleting succeeded jobs/pods")
	flag.DurationVar(&cfg.failedTTL, "failed-ttl", 0, "minimum age after failure before deleting failed jobs/pods")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "log verbosity level (debug, info, warn, error)")
	flag.StringVar(&cfg.logFormat, "log-format", "text", "log output format (text or json)")
	flag.Parse()

	cfg.excludeNamespaces = parseNamespaceSet(excludeCSV)

	if err := setupLogger(cfg.logLevel, cfg.logFormat); err != nil {
		slog.Error("invalid logger configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cs, authMode, err := buildClient(cfg.kubeconfig)
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	targetNS := cfg.namespace
	if targetNS == "" {
		targetNS = "<all>"
	}
	slog.Info("starting k8s-job-cleaner-go",
		"authMode", authMode,
		"namespace", targetNS,
		"excludedNamespaces", excludeCSV,
		"dryRun", cfg.dryRun,
		"ignoreNativeTTL", cfg.ignoreNativeTTL,
		"maxCount", cfg.maxCount,
		"succeededTTL", cfg.succeededTTL,
		"failedTTL", cfg.failedTTL,
		"logLevel", cfg.logLevel,
		"logFormat", cfg.logFormat,
	)

	if err := run(ctx, cs, cfg); err != nil {
		slog.Error("cleanup failed", "error", err)
		os.Exit(1)
	}
}
