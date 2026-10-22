package execution

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func Client(o Options) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes configuration: %w", err)
	}
	// No cfg.Timeout: it becomes http.Client.Timeout and would also cut followed
	// log streams. Short calls are bounded by per-call contexts instead.
	return kubernetes.NewForConfig(cfg)
}

// WorkerClient deliberately ignores all imported environment, including proxy,
// kubeconfig, service host, TLS and certificate overrides.
func WorkerClient() (kubernetes.Interface, error) {
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount"
	if _, err := os.Stat(filepath.Join(sa, "token")); err != nil {
		return nil, err
	}
	cfg := &rest.Config{Host: "https://kubernetes.default.svc", BearerTokenFile: filepath.Join(sa, "token"), TLSClientConfig: rest.TLSClientConfig{CAFile: filepath.Join(sa, "ca.crt")}, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	return kubernetes.NewForConfig(cfg)
}
