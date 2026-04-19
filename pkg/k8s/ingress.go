package k8s

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IngressSpec is what persistent + shared runtimes pass in to publish the
// worker Service externally. TLS is optional; if TLSSecret is empty we skip
// the TLS stanza.
type IngressSpec struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Host        string
	ServiceName string
	ServicePort int32
	TLSSecret   string
	ClassName   string
}

// CreateIngress creates a single-host Ingress that routes to the Service.
func (c *Client) CreateIngress(ctx context.Context, s IngressSpec) error {
	pt := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: s.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pt,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: s.ServiceName,
									Port: networkingv1.ServiceBackendPort{Number: s.ServicePort},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if s.ClassName != "" {
		ing.Spec.IngressClassName = &s.ClassName
	}
	if s.TLSSecret != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{s.Host},
			SecretName: s.TLSSecret,
		}}
	}
	if _, err := c.CS.NetworkingV1().Ingresses(s.Namespace).Create(ctx, ing, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create ingress: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
