package k8s

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// JobSpec is a minimal description of an opencode worker Job.
type JobSpec struct {
	Name         string
	Namespace    string
	Image        string
	Labels       map[string]string
	Env          map[string]string
	CPURequest   string
	CPULimit     string
	MemRequest   string
	MemLimit     string
	Port         int32
	Workdir      string
	BackoffLimit int32
}

// CreateJob creates a Job + matching ClusterIP Service. Service name == Job name.
func (c *Client) CreateJob(ctx context.Context, s JobSpec) error {
	if s.Port == 0 {
		s.Port = 4096
	}
	if s.BackoffLimit == 0 {
		s.BackoffLimit = 1
	}
	envVars := make([]corev1.EnvVar, 0, len(s.Env))
	for k, v := range s.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &s.BackoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: s.Labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "opencode",
						Image: s.Image,
						Env:   envVars,
						Ports: []corev1.ContainerPort{{ContainerPort: s.Port, Name: "http"}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(orDefault(s.CPURequest, "200m")),
								corev1.ResourceMemory: resource.MustParse(orDefault(s.MemRequest, "512Mi")),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(orDefault(s.CPULimit, "2")),
								corev1.ResourceMemory: resource.MustParse(orDefault(s.MemLimit, "4Gi")),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/app",
									Port: intstr.FromInt(int(s.Port)),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       3,
						},
					}},
				},
			},
		},
	}
	if _, err := c.CS.BatchV1().Jobs(s.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: s.Labels,
			Ports: []corev1.ServicePort{{
				Port:       s.Port,
				TargetPort: intstr.FromInt(int(s.Port)),
				Protocol:   corev1.ProtocolTCP,
			}},
			ClusterIP: corev1.ClusterIPNone, // headless: ready Pod gets DNS A record
		},
	}
	if _, err := c.CS.CoreV1().Services(s.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

// WaitForJobPodReady blocks until at least one Pod for the Job is Ready or
// the deadline expires. Returns the Pod name.
func (c *Client) WaitForJobPodReady(ctx context.Context, namespace, jobName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pods, err := c.CS.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + jobName,
		})
		if err == nil {
			for _, p := range pods.Items {
				for _, cnd := range p.Status.Conditions {
					if cnd.Type == corev1.PodReady && cnd.Status == corev1.ConditionTrue {
						return p.Name, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("pod for job %s not ready after %s", jobName, timeout)
}

// DeleteJob removes the Job and its Service.
func (c *Client) DeleteJob(ctx context.Context, namespace, name string) error {
	bg := metav1.DeletePropagationBackground
	_ = c.CS.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
	_ = c.CS.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
