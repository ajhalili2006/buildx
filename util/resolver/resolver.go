package resolver

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/moby/buildkit/util/tracing"
	"github.com/pkg/errors"
)

// TODO: copied from buildkit/util/resolver. Update upstream so we can use the same code.

type RegistryConfig struct {
	Mirrors   []string
	PlainHTTP *bool
	Insecure  *bool
	RootCAs   [][]byte
	KeyPairs  []TLSKeyPair
}

type TLSKeyPair struct {
	Key         []byte
	Certificate []byte
}

func fillInsecureOpts(host string, c RegistryConfig, h docker.RegistryHost) ([]docker.RegistryHost, error) {
	var hosts []docker.RegistryHost

	tc, err := loadTLSConfig(c)
	if err != nil {
		return nil, err
	}
	var isHTTP bool

	if c.PlainHTTP != nil && *c.PlainHTTP {
		isHTTP = true
	}
	if c.PlainHTTP == nil {
		if ok, _ := docker.MatchLocalhost(host); ok {
			isHTTP = true
		}
	}

	if isHTTP {
		h2 := h
		h2.Scheme = "http"
		hosts = append(hosts, h2)
	}
	if c.Insecure != nil && *c.Insecure {
		h2 := h
		transport := newDefaultTransport()
		transport.TLSClientConfig = tc
		h2.Client = &http.Client{
			Transport: tracing.NewTransport(transport),
		}
		tc.InsecureSkipVerify = true
		hosts = append(hosts, h2)
	}

	if len(hosts) == 0 {
		transport := newDefaultTransport()
		transport.TLSClientConfig = tc

		h.Client = &http.Client{
			Transport: tracing.NewTransport(transport),
		}
		hosts = append(hosts, h)
	}

	return hosts, nil
}

func loadTLSConfig(c RegistryConfig) (*tls.Config, error) {
	tc := &tls.Config{}
	if len(c.RootCAs) > 0 {
		systemPool, err := x509.SystemCertPool()
		if err != nil {
			if runtime.GOOS == "windows" {
				systemPool = x509.NewCertPool()
			} else {
				return nil, errors.Wrapf(err, "unable to get system cert pool")
			}
		}
		tc.RootCAs = systemPool
	}

	for _, p := range c.RootCAs {
		tc.RootCAs.AppendCertsFromPEM(p)
	}

	for _, kp := range c.KeyPairs {
		cert, err := tls.X509KeyPair(kp.Certificate, kp.Key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load keypair for %s", kp.Certificate)
		}
		tc.Certificates = append(tc.Certificates, cert)
	}
	return tc, nil
}

// NewRegistryConfig converts registry config to docker.RegistryHosts callback
func NewRegistryConfig(m map[string]RegistryConfig) docker.RegistryHosts {
	return docker.Registries(
		func(host string) ([]docker.RegistryHost, error) {
			c, ok := m[host]
			if !ok {
				return nil, nil
			}

			var out []docker.RegistryHost

			for _, mirror := range c.Mirrors {
				h := docker.RegistryHost{
					Scheme:       "https",
					Client:       newDefaultClient(),
					Host:         mirror,
					Path:         "/v2",
					Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
				}

				hosts, err := fillInsecureOpts(mirror, m[mirror], h)
				if err != nil {
					return nil, err
				}

				out = append(out, hosts...)
			}

			if host == "docker.io" {
				host = "registry-1.docker.io"
			}

			h := docker.RegistryHost{
				Scheme:       "https",
				Client:       newDefaultClient(),
				Host:         host,
				Path:         "/v2",
				Capabilities: docker.HostCapabilityPush | docker.HostCapabilityPull | docker.HostCapabilityResolve,
			}

			hosts, err := fillInsecureOpts(host, c, h)
			if err != nil {
				return nil, err
			}

			out = append(out, hosts...)
			return out, nil
		},
		docker.ConfigureDefaultRegistries(
			docker.WithClient(newDefaultClient()),
			docker.WithPlainHTTP(docker.MatchLocalhost),
		),
	)
}

func newDefaultClient() *http.Client {
	return &http.Client{
		Transport: tracing.NewTransport(newDefaultTransport()),
	}
}

// newDefaultTransport is for pull or push client
//
// NOTE: For push, there must disable http2 for https because the flow control
// will limit data transfer. The net/http package doesn't provide http2 tunable
// settings which limits push performance.
//
// REF: https://github.com/golang/go/issues/14077
func newDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		MaxIdleConns:          30,
		IdleConnTimeout:       120 * time.Second,
		MaxIdleConnsPerHost:   4,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
	}
}
