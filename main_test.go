package main

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func makeJob(name, ns string, cond batchv1.JobConditionType, finished time.Time, active int32, nativeTTL *int32) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(finished.Add(-10 * time.Minute)),
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: nativeTTL,
		},
	}
	j.Status.Active = active
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{
			Type:               cond,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(finished),
		}}
	}
	return j
}

func TestEvaluateJob(t *testing.T) {
	now := time.Now()
	ttlSec := int32(300)
	cfg := config{
		excludeNamespaces: parseNamespaceSet("kube-system,kube-public"),
		succeededTTL:      time.Hour,
		failedTTL:         24 * time.Hour,
	}

	tests := []struct {
		name       string
		job        *batchv1.Job
		wantDelete bool
		wantReason string
	}{
		{"active", makeJob("a", "default", "", now, 1, nil), false, "active_or_incomplete"},
		{"succeeded old", makeJob("b", "default", batchv1.JobComplete, now.Add(-2*time.Hour), 0, nil), true, "retention_ttl_expired"},
		{"succeeded recent", makeJob("c", "default", batchv1.JobComplete, now.Add(-time.Minute), 0, nil), false, "within_retention_ttl"},
		{"failed within ttl", makeJob("d", "default", batchv1.JobFailed, now.Add(-2*time.Hour), 0, nil), false, "within_retention_ttl"},
		{"failed old", makeJob("e", "default", batchv1.JobFailed, now.Add(-48*time.Hour), 0, nil), true, "retention_ttl_expired"},
		{"excluded namespace", makeJob("f", "kube-system", batchv1.JobComplete, now.Add(-5*time.Hour), 0, nil), false, "excluded_namespace"},
		{"native ttl set", makeJob("g", "default", batchv1.JobComplete, now.Add(-5*time.Hour), 0, &ttlSec), false, "managed_by_native_ttl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := evaluateJob(tt.job, cfg, now)
			if dec.deletable != tt.wantDelete || dec.reason != tt.wantReason {
				t.Errorf("evaluateJob() = (%v, %q), want (%v, %q)", dec.deletable, dec.reason, tt.wantDelete, tt.wantReason)
			}
		})
	}
}

func TestEvaluatePod(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	cfg := config{
		excludeNamespaces: parseNamespaceSet("kube-system"),
		succeededTTL:      time.Hour,
		failedTTL:         time.Hour,
	}

	standalonePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default", CreationTimestamp: metav1.NewTime(old)},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	if dec := evaluatePod(standalonePod, cfg, now); !dec.deletable || dec.reason != "retention_ttl_expired" {
		t.Fatalf("expected standalone pod to be deletable, got %+v", dec)
	}

	jobOwnedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "job-pod",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(old),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       "parent-job",
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	if dec := evaluatePod(jobOwnedPod, cfg, now); dec.deletable || dec.reason != "owned_by_job" {
		t.Fatalf("expected job-owned pod to be skipped, got %+v", dec)
	}

	initFailedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-failed", Namespace: "default", CreationTimestamp: metav1.NewTime(old)},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(now.Add(-10 * time.Minute))},
				},
			}},
		},
	}
	if dec := evaluatePod(initFailedPod, cfg, now); dec.deletable || dec.reason != "within_retention_ttl" {
		t.Fatalf("expected init-failed pod with recent init container finish to be retained, got %+v", dec)
	}
}

func TestRunDeletesAndDryRun(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", CreationTimestamp: metav1.NewTime(old)},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	sysJob := makeJob("sys-job", "kube-system", batchv1.JobComplete, old, 0, nil)
	userJob := makeJob("done", "default", batchv1.JobComplete, old, 0, nil)
	cs := fake.NewClientset(userJob, sysJob, pod)

	cfg := config{
		maxCount:          10,
		dryRun:            true,
		excludeNamespaces: parseNamespaceSet("kube-system"),
	}
	if err := run(context.Background(), cs, cfg); err != nil {
		t.Fatal(err)
	}
	if l, _ := cs.BatchV1().Jobs("default").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 1 {
		t.Fatal("dry-run must not delete jobs")
	}

	cfg.dryRun = false
	if err := run(context.Background(), cs, cfg); err != nil {
		t.Fatal(err)
	}
	if l, _ := cs.BatchV1().Jobs("default").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatal("user job should be deleted")
	}
	if l, _ := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 1 {
		t.Fatal("kube-system job should be protected")
	}
	if l, _ := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatal("standalone pod should be deleted")
	}
}
