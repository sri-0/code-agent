package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DeploymentSpec describes a long-running worker (used by persistent + shared
// runtimes). Service is always created with the same name; an Ingress is
// created when IngressHost is non-empty.
type DeploymentSpec struct {
	Name      string
	Namespace string
	Image     string
	Labels    map[string]string
	Env       map[string]string
	Replicas  int32

	CPURequest string
	CPULimit   string
	MemRequest string
	MemLimit   string

	Port      int32
	AdminPort int32 // 0 disables admin port

	// Ingress (optional) — routes only to Port (opencode), not AdminPort.
	IngressHost      string
	IngressTLSSecret string
	IngressClassName string

	// SecretMounts projects k8s Secrets into the pod filesystem. Used for
	// per-repo .env files (one Secret per repo). Each mount lands at
	// MountPath as a directory containing the Secret's keys as files.
	SecretMounts []SecretMount
}

// SecretMount projects one k8s Secret into the worker pod. The Secret
// must exist in the same namespace; missing Secrets cause the pod to
// hang in ContainerCreating until the orchestrator creates them.
type SecretMount struct {
	// Name uniquely identifies this mount within the spec; used as the
	// volume name. Letters/digits/dashes only.
	Name string
	// SecretName is the k8s Secret resource name.
	SecretName string
	// MountPath is where the secret's keys appear (e.g. /secrets/risky-api).
	MountPath string
	// Optional — if true, missing Secret won't block pod startup. Use
	// only when the Secret may legitimately not exist yet.
	Optional bool
}

// CreateDeployment creates Deployment + ClusterIP Service + optional Ingress.
// Idempotent-ish: returns an error if any already exists (caller should
// check before).
func (c *Client) CreateDeployment(ctx context.Context, s DeploymentSpec) error {
	if s.Port == 0 {
		s.Port = 4096
	}
	if s.Replicas == 0 {
		s.Replicas = 1
	}

	envVars := make([]corev1.EnvVar, 0, len(s.Env))
	for k, v := range s.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	volumes := make([]corev1.Volume, 0, len(s.SecretMounts))
	mounts := make([]corev1.VolumeMount, 0, len(s.SecretMounts))
	for _, m := range s.SecretMounts {
		opt := m.Optional
		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: m.SecretName,
					Optional:   &opt,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ReadOnly:  true,
		})
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &s.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: s.Labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: s.Labels},
				Spec: corev1.PodSpec{
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:         "opencode",
						Image:        s.Image,
						Env:          envVars,
						Ports:        containerPorts(s.Port, s.AdminPort),
						VolumeMounts: mounts,
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
	if _, err := c.CS.AppsV1().Deployments(s.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: s.Labels,
			Ports:    servicePorts(s.Port, s.AdminPort),
		},
	}
	if _, err := c.CS.CoreV1().Services(s.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	if s.IngressHost != "" {
		if err := c.CreateIngress(ctx, IngressSpec{
			Name:       s.Name,
			Namespace:  s.Namespace,
			Labels:     s.Labels,
			Host:       s.IngressHost,
			ServiceName: s.Name,
			ServicePort: s.Port,
			TLSSecret:  s.IngressTLSSecret,
			ClassName:  s.IngressClassName,
		}); err != nil {
			return err
		}
	}
	return nil
}

// WaitForDeploymentReady blocks until at least one Pod is Ready.
func (c *Client) WaitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := c.CS.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && dep.Status.ReadyReplicas > 0 {
			// return the first ready pod
			pods, err := c.CS.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selectorFromLabels(dep.Spec.Selector.MatchLabels),
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
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("deployment %s/%s not ready after %s", namespace, name, timeout)
}

// DeleteDeployment removes Deployment + Service + Ingress by name.
func (c *Client) DeleteDeployment(ctx context.Context, namespace, name string) error {
	bg := metav1.DeletePropagationBackground
	_ = c.CS.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
	_ = c.CS.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.CS.NetworkingV1().Ingresses(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	return nil
}

// DeploymentExists is a cheap presence check.
func (c *Client) DeploymentExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := c.CS.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func containerPorts(httpPort, adminPort int32) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{{ContainerPort: httpPort, Name: "http"}}
	if adminPort > 0 {
		ports = append(ports, corev1.ContainerPort{ContainerPort: adminPort, Name: "admin"})
	}
	return ports
}

func servicePorts(httpPort, adminPort int32) []corev1.ServicePort {
	ports := []corev1.ServicePort{{
		Name:       "http",
		Port:       httpPort,
		TargetPort: intstr.FromInt(int(httpPort)),
		Protocol:   corev1.ProtocolTCP,
	}}
	if adminPort > 0 {
		ports = append(ports, corev1.ServicePort{
			Name:       "admin",
			Port:       adminPort,
			TargetPort: intstr.FromInt(int(adminPort)),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

func selectorFromLabels(m map[string]string) string {
	var b strings.Builder
	first := true
	for k, v := range m {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		first = false
	}
	return b.String()
}
