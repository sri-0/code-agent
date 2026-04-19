// Package k8s wraps client-go with the small surface the orchestrator needs:
// in-cluster or kubeconfig client init, plus typed helpers for the
// resources we create (Jobs, Deployments, Services, Ingresses, Secrets).
package k8s

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles the typed kubernetes client.
type Client struct {
	CS        *kubernetes.Clientset
	Namespace string
	InCluster bool
}

// New constructs a client. If KUBECONFIG or ~/.kube/config exists and
// KUBERNETES_SERVICE_HOST isn't set, we use the kubeconfig path; otherwise
// we use in-cluster config.
func New(namespace string) (*Client, error) {
	cfg, inCluster, err := loadConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	if namespace == "" {
		namespace = "default"
	}
	return &Client{CS: cs, Namespace: namespace, InCluster: inCluster}, nil
}

func loadConfig() (*rest.Config, bool, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBECONFIG") == "" {
		c, err := rest.InClusterConfig()
		return c, true, err
	}
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		home, _ := os.UserHomeDir()
		kc = filepath.Join(home, ".kube", "config")
	}
	c, err := clientcmd.BuildConfigFromFlags("", kc)
	return c, false, err
}
