package main

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	maxCount = 100
)

func isDeletableJob(job batchv1.Job) bool {
	// Is Job marked as "Succeeded" ?
	if job.Status.Active == 0 {
		// fmt.Printf("Job is active, not deletable.\n")
		for _, condition := range job.Status.Conditions {
			conditionType := condition.Type
			// fmt.Printf("%+v\n", conditionType)
			if conditionType ==  batchv1.JobComplete {
				fmt.Printf("Job is active with Completed state. Deletable.\n")
				return true
			}
			if conditionType ==  batchv1.JobFailed {
				fmt.Printf("Job is active with Failed state. Deletable.\n")
				return true
			}
		}
		return false
	}
	if job.Status.Succeeded > 0 {
		fmt.Printf("Job is Success, deletable.\n")
		return true
	}
	return false
}

func main() {
	var (
		// namespace string
		podCount int64
		jobCount int64
	)

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	jobs, err := clientset.BatchV1().Jobs("").List(metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("There are %d jobs in the cluster\n", len(jobs.Items))

	for idx, job := range jobs.Items {
	// for _, job := range jobs.Items {
		fmt.Printf(" %d ::: %s ::: %d \n", idx, job.Name, job.Status.Succeeded)
		if isDeletableJob(job){
			// jobGroup = append(jobGroup, job)
			jobCount = jobCount + 1
			fmt.Printf(" Deleting job ::: %s ::: %d \n", job.Name, job.Status.Succeeded)
			if err := clientset.BatchV1().Jobs(job.Namespace).Delete(job.Name, &metav1.DeleteOptions{}); err != nil {
				fmt.Printf("failed to delete Job ::: %s \n", string(err.Error()))
			}
		} else {
			continue
		}
		if jobCount >= maxCount {
			break
		}
	}
	fmt.Printf(" Deletable Jobs count : %d \n", jobCount)
	_ = jobCount


	pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("There are %d pods in the cluster\n", len(pods.Items))

	for idx, pod := range pods.Items {
		fmt.Printf(" %d ::: %s ::: %s \n", idx, pod.Name, pod.Status.Phase)
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			podCount = podCount + 1
			fmt.Printf(" Deleting pod ::: %s ::: %s  \n", pod.Name, pod.Status.Phase)
			if err := clientset.CoreV1().Pods(pod.Namespace).Delete(pod.Name, &metav1.DeleteOptions{}); err != nil {
				fmt.Printf("failed to delete Pod ::: %s \n", string(err.Error()))
			}
		} else {
			continue
		}
		if podCount >= maxCount {
			break
		}
	}
	fmt.Printf(" Deletable Pods count : %d \n", podCount)
	_ = podCount
}
