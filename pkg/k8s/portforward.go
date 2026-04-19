package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForward is a live kubectl-style port-forward from a pod. It stays
// open until Close() is called. LocalPorts maps each remote port
// requested to the local port assigned by the forwarder.
type PortForward struct {
	LocalPort  int         // convenience: first (often only) forwarded port
	LocalPorts map[int]int // remote -> local for all forwarded ports
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
}

// Close tears down the tunnel and waits for the forwarder goroutine to
// exit.
func (p *PortForward) Close() {
	p.once.Do(func() {
		close(p.stop)
		<-p.done
	})
}

// ForwardPodPort is a single-port convenience wrapper around
// ForwardPodPorts. Kept for callers that only need one port.
func (c *Client) ForwardPodPort(ctx context.Context, namespace, podName string, remotePort int) (*PortForward, error) {
	return c.ForwardPodPorts(ctx, namespace, podName, []int{remotePort})
}

// ForwardPodPorts opens an SPDY port-forward to one or more pod ports.
// Returns a PortForward with LocalPorts mapping remote->local. LocalPort
// convenience field is set to the first requested port's local.
//
// Used by pod-based runtimes when the orchestrator is running outside
// the cluster: svc.cluster.local DNS is unreachable from the host, so we
// tunnel via the kube-apiserver.
func (c *Client) ForwardPodPorts(ctx context.Context, namespace, podName string, remotePorts []int) (*PortForward, error) {
	if len(remotePorts) == 0 {
		return nil, fmt.Errorf("at least one remote port required")
	}
	// Build the URL to POST to the apiserver's portforward subresource.
	cfgHost, err := apiServerURL(c)
	if err != nil {
		return nil, err
	}
	u := &url.URL{
		Scheme: cfgHost.Scheme,
		Host:   cfgHost.Host,
		Path: fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward",
			namespace, podName),
	}

	rt, upgrader, err := spdy.RoundTripperFor(c.restConfig)
	if err != nil {
		return nil, fmt.Errorf("spdy roundtripper: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)

	stop := make(chan struct{})
	ready := make(chan struct{})
	ports := make([]string, 0, len(remotePorts))
	for _, rp := range remotePorts {
		ports = append(ports, fmt.Sprintf("0:%d", rp)) // 0 = pick a free local port
	}
	fw, err := portforward.New(dialer, ports, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("portforward.New: %w", err)
	}

	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		if err := fw.ForwardPorts(); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	// Wait for the forwarder to say ready (or fail early).
	select {
	case <-ready:
	case err := <-errCh:
		return nil, fmt.Errorf("port-forward: %w", err)
	case <-time.After(30 * time.Second):
		close(stop)
		<-done
		return nil, fmt.Errorf("port-forward: timed out waiting for ready")
	case <-ctx.Done():
		close(stop)
		<-done
		return nil, ctx.Err()
	}

	actual, err := fw.GetPorts()
	if err != nil || len(actual) == 0 {
		close(stop)
		<-done
		return nil, fmt.Errorf("port-forward: get ports: %w", err)
	}

	localPorts := make(map[int]int, len(actual))
	for _, p := range actual {
		localPorts[int(p.Remote)] = int(p.Local)
	}

	return &PortForward{
		LocalPort:  int(actual[0].Local),
		LocalPorts: localPorts,
		stop:       stop,
		done:       done,
	}, nil
}

// apiServerURL extracts scheme+host from the client's rest config.
func apiServerURL(c *Client) (*url.URL, error) {
	raw := c.restConfig.Host
	// rest.Config.Host is typically a full URL; sometimes just host:port.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse apiserver url %q: %w", raw, err)
	}
	return u, nil
}

// Unused but kept for the package to satisfy any future reference; avoids
// dead-code warnings when the surrounding runtime iterates on flags.
var _ = metav1.ListOptions{}
var _ = strconv.Itoa
