// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	stdnet "net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	containerdapi "github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	cni "github.com/containerd/go-cni"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/talos-systems/net"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"

	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/events"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/health"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/runner"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/runner/containerd"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/runner/restart"
	"github.com/talos-systems/talos/internal/pkg/capability"
	"github.com/talos-systems/talos/internal/pkg/containers/image"
	"github.com/talos-systems/talos/pkg/argsbuilder"
	"github.com/talos-systems/talos/pkg/conditions"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1/machine"
	"github.com/talos-systems/talos/pkg/machinery/constants"
	"github.com/talos-systems/talos/pkg/machinery/resources/k8s"
	"github.com/talos-systems/talos/pkg/machinery/resources/network"
	timeresource "github.com/talos-systems/talos/pkg/machinery/resources/time"
)

var kubeletKubeConfigTemplate = []byte(`apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: {{ .Server }}
    certificate-authority-data: {{ .CACert }}
users:
- name: kubelet
  user:
    token: {{ .BootstrapTokenID }}.{{ .BootstrapTokenSecret }}
contexts:
- context:
    cluster: local
    user: kubelet
`)

// Kubelet implements the Service interface. It serves as the concrete type with
// the required methods.
type Kubelet struct{}

// ID implements the Service interface.
func (k *Kubelet) ID(r runtime.Runtime) string {
	return "kubelet"
}

// PreFunc implements the Service interface.
func (k *Kubelet) PreFunc(ctx context.Context, r runtime.Runtime) error {
	cfg := struct {
		Server               string
		CACert               string
		BootstrapTokenID     string
		BootstrapTokenSecret string
	}{
		Server:               r.Config().Cluster().Endpoint().String(),
		CACert:               base64.StdEncoding.EncodeToString(r.Config().Cluster().CA().Crt),
		BootstrapTokenID:     r.Config().Cluster().Token().ID(),
		BootstrapTokenSecret: r.Config().Cluster().Token().Secret(),
	}

	templ := template.Must(template.New("tmpl").Parse(string(kubeletKubeConfigTemplate)))

	var buf bytes.Buffer

	if err := templ.Execute(&buf, cfg); err != nil {
		return err
	}

	if err := ioutil.WriteFile(constants.KubeletBootstrapKubeconfig, buf.Bytes(), 0o600); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(constants.KubernetesCACert), 0o700); err != nil {
		return err
	}

	if err := ioutil.WriteFile(constants.KubernetesCACert, r.Config().Cluster().CA().Crt, 0o400); err != nil {
		return err
	}

	if err := writeKubeletConfig(r); err != nil {
		return err
	}

	client, err := containerdapi.New(constants.CRIContainerdAddress)
	if err != nil {
		return err
	}
	//nolint:errcheck
	defer client.Close()

	// Pull the image and unpack it.
	containerdctx := namespaces.WithNamespace(ctx, constants.SystemContainerdNamespace)

	_, err = image.Pull(containerdctx, r.Config().Machine().Registries(), client, r.Config().Machine().Kubelet().Image(), image.WithSkipIfAlreadyPulled())
	if err != nil {
		return err
	}

	return nil
}

// PostFunc implements the Service interface.
func (k *Kubelet) PostFunc(r runtime.Runtime, state events.ServiceState) (err error) {
	return nil
}

// Condition implements the Service interface.
func (k *Kubelet) Condition(r runtime.Runtime) conditions.Condition {
	return conditions.WaitForAll(
		timeresource.NewSyncCondition(r.State().V1Alpha2().Resources()),
		network.NewReadyCondition(r.State().V1Alpha2().Resources(), network.AddressReady, network.HostnameReady, network.EtcFilesReady),
		k8s.NewNodenameReadyCondition(r.State().V1Alpha2().Resources()),
	)
}

// DependsOn implements the Service interface.
func (k *Kubelet) DependsOn(r runtime.Runtime) []string {
	return []string{"cri"}
}

// Runner implements the Service interface.
func (k *Kubelet) Runner(r runtime.Runtime) (runner.Runner, error) {
	a, err := k.args(r)
	if err != nil {
		return nil, err
	}

	// Set the process arguments.
	args := runner.Args{
		ID:          k.ID(r),
		ProcessArgs: append([]string{"/usr/local/bin/kubelet"}, a...),
	}
	// Set the required kubelet mounts.
	mounts := []specs.Mount{
		{Type: "bind", Destination: "/dev", Source: "/dev", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "sysfs", Destination: "/sys", Source: "/sys", Options: []string{"bind", "ro"}},
		{Type: "bind", Destination: constants.CgroupMountPath, Source: constants.CgroupMountPath, Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/lib/modules", Source: "/lib/modules", Options: []string{"bind", "ro"}},
		{Type: "bind", Destination: "/etc/kubernetes", Source: "/etc/kubernetes", Options: []string{"bind", "rshared", "rw"}},
		{Type: "bind", Destination: "/etc/os-release", Source: "/etc/os-release", Options: []string{"bind", "ro"}},
		{Type: "bind", Destination: "/etc/cni", Source: "/etc/cni", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/usr/libexec/kubernetes", Source: "/usr/libexec/kubernetes", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/var/run", Source: "/run", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/var/lib/containerd", Source: "/var/lib/containerd", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/var/lib/kubelet", Source: "/var/lib/kubelet", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/var/log/containers", Source: "/var/log/containers", Options: []string{"rbind", "rshared", "rw"}},
		{Type: "bind", Destination: "/var/log/pods", Source: "/var/log/pods", Options: []string{"rbind", "rshared", "rw"}},
	}

	// Add extra mounts.
	// TODO(andrewrynhard): We should verify that the mount source is
	// allowlisted. There is the potential that a user can expose
	// sensitive information.
	for _, mount := range r.Config().Machine().Kubelet().ExtraMounts() {
		if err = os.MkdirAll(mount.Source, 0o700); err != nil {
			return nil, err
		}

		mounts = append(mounts, mount)
	}

	env := []string{}
	for key, val := range r.Config().Machine().Env() {
		env = append(env, fmt.Sprintf("%s=%s", key, val))
	}

	return restart.New(containerd.NewRunner(
		r.Config().Debug() && r.Config().Machine().Type() == machine.TypeWorker, // enable debug logs only for the worker nodes
		&args,
		runner.WithLoggingManager(r.Logging()),
		runner.WithNamespace(constants.SystemContainerdNamespace),
		runner.WithContainerImage(r.Config().Machine().Kubelet().Image()),
		runner.WithEnv(env),
		runner.WithOCISpecOpts(
			containerd.WithRootfsPropagation("shared"),
			oci.WithCgroup(constants.CgroupKubelet),
			oci.WithMounts(mounts),
			oci.WithHostNamespace(specs.NetworkNamespace),
			oci.WithHostNamespace(specs.PIDNamespace),
			oci.WithParentCgroupDevices,
			oci.WithMaskedPaths(nil),
			oci.WithReadonlyPaths(nil),
			oci.WithWriteableSysfs,
			oci.WithWriteableCgroupfs,
			oci.WithSelinuxLabel(""),
			oci.WithApparmorProfile(""),
			oci.WithAllDevicesAllowed,
			oci.WithCapabilities(capability.AllGrantableCapabilities()), // TODO: kubelet doesn't need all of these, we should consider limiting capabilities
		),
		runner.WithOOMScoreAdj(constants.KubeletOOMScoreAdj),
		runner.WithCustomSeccompProfile(kubeletSeccomp),
	),
		restart.WithType(restart.Forever),
	), nil
}

// HealthFunc implements the HealthcheckedService interface.
func (k *Kubelet) HealthFunc(runtime.Runtime) health.Check {
	return func(ctx context.Context) error {
		req, err := http.NewRequest("GET", "http://127.0.0.1:10248/healthz", nil)
		if err != nil {
			return err
		}

		req = req.WithContext(ctx)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		//nolint:errcheck
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("expected HTTP status OK, got %s", resp.Status)
		}

		return nil
	}
}

// HealthSettings implements the HealthcheckedService interface.
func (k *Kubelet) HealthSettings(runtime.Runtime) *health.Settings {
	settings := health.DefaultSettings
	settings.InitialDelay = 2 * time.Second // increase initial delay as kubelet is slow on startup

	return &settings
}

// APIRestartAllowed implements APIRestartableService.
func (k *Kubelet) APIRestartAllowed(runtime.Runtime) bool {
	return true
}

func newKubeletConfiguration(clusterDNS []string, dnsDomain string) *kubeletconfig.KubeletConfiguration {
	f := false
	t := true
	oomScoreAdj := int32(constants.KubeletOOMScoreAdj)

	return &kubeletconfig.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kubelet.config.k8s.io/v1beta1",
			Kind:       "KubeletConfiguration",
		},
		StaticPodPath:      constants.ManifestsDirectory,
		Address:            "0.0.0.0",
		Port:               constants.KubeletPort,
		OOMScoreAdj:        &oomScoreAdj,
		RotateCertificates: true,
		Authentication: kubeletconfig.KubeletAuthentication{
			X509: kubeletconfig.KubeletX509Authentication{
				ClientCAFile: constants.KubernetesCACert,
			},
			Webhook: kubeletconfig.KubeletWebhookAuthentication{
				Enabled: &t,
			},
			Anonymous: kubeletconfig.KubeletAnonymousAuthentication{
				Enabled: &f,
			},
		},
		Authorization: kubeletconfig.KubeletAuthorization{
			Mode: kubeletconfig.KubeletAuthorizationModeWebhook,
		},
		ClusterDomain:       dnsDomain,
		ClusterDNS:          clusterDNS,
		SerializeImagePulls: &f,
		FailSwapOn:          &f,
		CgroupRoot:          "/",
		SystemCgroups:       constants.CgroupSystem,
		SystemReserved: map[string]string{
			"cpu":               constants.KubeletSystemReservedCPU,
			"memory":            constants.KubeletSystemReservedMemory,
			"pid":               constants.KubeletSystemReservedPid,
			"ephemeral-storage": constants.KubeletSystemReservedEphemeralStorage,
		},
		KubeletCgroups: constants.CgroupKubelet,
	}
}

//nolint:gocyclo
func (k *Kubelet) args(r runtime.Runtime) ([]string, error) {
	nodename, err := r.NodeName()
	if err != nil {
		return nil, err
	}

	args := argsbuilder.Args{
		"bootstrap-kubeconfig":       constants.KubeletBootstrapKubeconfig,
		"kubeconfig":                 constants.KubeletKubeconfig,
		"container-runtime":          "remote",
		"container-runtime-endpoint": "unix://" + constants.CRIContainerdAddress,
		"config":                     "/etc/kubernetes/kubelet.yaml",

		"cert-dir":     constants.KubeletPKIDir,
		"cni-conf-dir": cni.DefaultNetDir,

		"hostname-override": nodename,

		"logging-format": "json",
	}

	if r.Config().Cluster().ExternalCloudProvider().Enabled() {
		args["cloud-provider"] = "external"
	}

	extraArgs := argsbuilder.Args(r.Config().Machine().Kubelet().ExtraArgs())

	validSubnets := r.Config().Machine().Kubelet().NodeIP().ValidSubnets()

	// configure automatically valid subnets for IPv4/IPv6 based on service CIDRs
	if len(validSubnets) == 0 {
		validSubnets, err = ipSubnetsFromServiceCIDRs(r.Config().Cluster().Network().ServiceCIDRs())
		if err != nil {
			return nil, err
		}
	}

	// anyway filter out pod cidrs, they can't be node IPs
	for _, cidr := range r.Config().Cluster().Network().PodCIDRs() {
		validSubnets = append(validSubnets, "!"+cidr)
	}

	// filter out any virtual IPs, they can't be node IPs either
	for _, device := range r.Config().Machine().Network().Devices() {
		if device.VIPConfig() != nil {
			validSubnets = append(validSubnets, "!"+device.VIPConfig().IP())
		}

		for _, vlan := range device.Vlans() {
			if vlan.VIPConfig() != nil {
				validSubnets = append(validSubnets, "!"+vlan.VIPConfig().IP())
			}
		}
	}

	// if the user supplied node-ip via extra args, no need to pick automatically
	if !extraArgs.Contains("node-ip") {
		var nodeIPs []stdnet.IP

		nodeIPs, err = pickNodeIPs(validSubnets)
		if err != nil {
			return nil, err
		}

		if len(nodeIPs) > 0 {
			nodeIPsString := make([]string, len(nodeIPs))

			for i := range nodeIPs {
				nodeIPsString[i] = nodeIPs[i].String()
			}

			args["node-ip"] = strings.Join(nodeIPsString, ",")
		}
	}

	if err = args.Merge(extraArgs, argsbuilder.WithMergePolicies(
		argsbuilder.MergePolicies{
			"bootstrap-kubeconfig":       argsbuilder.MergeDenied,
			"kubeconfig":                 argsbuilder.MergeDenied,
			"container-runtime":          argsbuilder.MergeDenied,
			"container-runtime-endpoint": argsbuilder.MergeDenied,
			"config":                     argsbuilder.MergeDenied,
			"cert-dir":                   argsbuilder.MergeDenied,
			"cni-conf-dir":               argsbuilder.MergeDenied,
		},
	)); err != nil {
		return nil, err
	}

	return args.Args(), nil
}

func writeKubeletConfig(r runtime.Runtime) error {
	dnsServiceIPs, err := r.Config().Cluster().Network().DNSServiceIPs()
	if err != nil {
		return fmt.Errorf("failed to get DNS service IPs: %w", err)
	}

	dnsServiceIPsString := []string{}

	dnsServiceIPsCustom := r.Config().Machine().Kubelet().ClusterDNS()
	if dnsServiceIPsCustom == nil {
		for _, dnsIP := range dnsServiceIPs {
			dnsServiceIPsString = append(dnsServiceIPsString, dnsIP.String())
		}
	} else {
		dnsServiceIPsString = dnsServiceIPsCustom
	}

	kubeletConfiguration := newKubeletConfiguration(dnsServiceIPsString, r.Config().Cluster().Network().DNSDomain())

	serializer := json.NewSerializerWithOptions(
		json.DefaultMetaFactory,
		nil,
		nil,
		json.SerializerOptions{
			Yaml:   true,
			Pretty: true,
			Strict: true,
		},
	)

	var buf bytes.Buffer

	if err := serializer.Encode(kubeletConfiguration, &buf); err != nil {
		return err
	}

	return ioutil.WriteFile("/etc/kubernetes/kubelet.yaml", buf.Bytes(), 0o600)
}

func ipSubnetsFromServiceCIDRs(serviceCIDRs []string) ([]string, error) {
	// automatically configure valid IP subnets based on service CIDRs
	// if the primary service CIDR is IPv4, primary kubelet node IP should be IPv4 as well, and so on
	result := make([]string, 0, len(serviceCIDRs))

	for _, cidr := range serviceCIDRs {
		network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse subnet: %w", err)
		}

		if network.IP.To4() == nil {
			result = append(result, "::/0")
		} else {
			result = append(result, "0.0.0.0/0")
		}
	}

	return result, nil
}

func pickNodeIPs(cidrs []string) ([]stdnet.IP, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}

	ips, err := net.IPAddrs()
	if err != nil {
		return nil, fmt.Errorf("failed to discover interface IP addresses: %w", err)
	}

	ips = net.IPFilter(ips, network.NotSideroLinkStdIP)

	ips, err = net.FilterIPs(ips, cidrs)
	if err != nil {
		return nil, err
	}

	// filter down to make sure only one IPv4 and one IPv6 address stays
	var hasIPv4, hasIPv6 bool

	result := make([]stdnet.IP, 0, 2)

	for _, ip := range ips {
		switch {
		case ip.To4() != nil:
			if !hasIPv4 {
				result = append(result, ip)
				hasIPv4 = true
			} else {
				log.Printf("kubelet: warning: skipped node IP %s, please use .machine.kubelet.nodeIP to provide explicit subnet for the node IP", ip)
			}
		case ip.To16() != nil:
			if !hasIPv6 {
				result = append(result, ip)
				hasIPv6 = true
			} else {
				log.Printf("kubelet: warning: skipped node IP %s, please use .machine.kubelet.nodeIP to provide explicit subnet for the node IP", ip)
			}
		}
	}

	return result, nil
}

func kubeletSeccomp(seccomp *specs.LinuxSeccomp) {
	// for cephfs mounts
	seccomp.Syscalls = append(seccomp.Syscalls,
		specs.LinuxSyscall{
			Names: []string{
				"add_key",
				"request_key",
			},
			Action: specs.ActAllow,
			Args:   []specs.LinuxSeccompArg{},
		},
	)
}
