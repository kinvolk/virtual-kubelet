package tinc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cpuguy83/strongerrors"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"

	"github.com/virtual-kubelet/virtual-kubelet/providers"
)

const (
	tincAutoConnect string = "yes"

	tincModeRouter string = "router"
	tincModeSwitch string = "switch"

	tincDeviceTunTap string = "/dev/net/tun"

	tincDeviceTypeDummy string = "dummy"
	tincDeviceTypeTap   string = "tap"

	tincImageName string = "quay.io/dongsupark/tinc"

	dockerClient = "/usr/bin/docker"

	// Provider configuration defaults.
	defaultCPUCapacity    = "20"
	defaultMemoryCapacity = "100Gi"
	defaultPodCapacity    = "20"

	// DefaultTincMainAddress is a name of the main node
	DefaultTincMainName string = "nodemain"

	// DefaultTincRemotePeers a remote hostname to be connected
	DefaultTincRemotePeers string = "nodepeer"

	// DefaultTincMainAddress is a public address of the main node
	DefaultTincMainAddress string = "172.17.0.2"
	// DefaultTincPeerAddress is a public address of the peer node
	DefaultTincPeerAddress string = "172.17.0.3"

	// DefaultTincMainAddress is a public address of the main node
	DefaultTincMainPrivateAddress string = "10.1.1.1"
	// DefaultTincPeerAddress is a public address of the peer node
	DefaultTincPeerPrivateAddress string = "10.1.1.2"

	// DefaultTincSubnet is a subnetwork for the test nodes
	DefaultTincSubnet string = "10.1.1.0/24"

	// DefaultTincPort is the default port number Tinc VPN listens on
	DefaultTincPort int32 = 655
)

var (
	myAddress      string = DefaultTincMainAddress
	peerAddress    string = DefaultTincPeerAddress
	privateAddress string = DefaultTincMainPrivateAddress

	tincMainName string = DefaultTincMainName
	tincPeerName string = DefaultTincRemotePeers

	tincStartupConfigHost      string = ""
	tincStartupConfigContainer string = "/environment/default.startup.conf"

	tincMainConfigHost      string = ""
	tincMainConfigContainer string = "/service/tinc/data/tinc.conf"

	tincUpScriptHost      string = ""
	tincUpScriptContainer string = "/service/tinc/data/tinc-up"
)

// TincProvider implements the virtual-kubelet provider interface and stores pods in memory.
type TincProvider struct {
	nodeName    string
	pods        map[string]*v1.Pod
	tincAddress string
	tincSubnet  string
	tincPort    int32
	config      TincConfig
}

// TincConfig contains a tinc virtual-kubelet's configurable parameters.
type TincConfig struct {
	AutoConnect string `json:"autoconnect,omitempty"`
	ConnectTo   string `json:"connect,omitempty"`
	Device      string `json:"device,omitempty"`
	DeviceType  string `json:"devicetype,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Name        string `json:"name,omitempty"`

	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   string `json:"pods,omitempty"`
}

// NewTincProvider creates a new TincProvider
func NewTincProvider(providerConfig, nodeName, tincAddress, tincSubnet string, tincPort int32) (*TincProvider, error) {
	config, err := loadConfig(providerConfig, nodeName)
	if err != nil {
		return nil, err
	}

	provider := TincProvider{
		nodeName:    nodeName,
		tincAddress: tincAddress,
		tincSubnet:  tincSubnet,
		tincPort:    tincPort,
		pods:        make(map[string]*v1.Pod),
		config:      config,
	}

	return &provider, nil
}

// loadConfig loads the given json configuration files.

func loadConfig(providerConfig, nodeName string) (config TincConfig, err error) {
	data, err := ioutil.ReadFile(providerConfig)
	if err != nil {
		return config, err
	}
	configMap := map[string]TincConfig{}
	err = json.Unmarshal(data, &configMap)
	if err != nil {
		return config, err
	}
	if _, exist := configMap[nodeName]; exist {
		config = configMap[nodeName]
		if config.AutoConnect == "" {
			config.AutoConnect = tincAutoConnect
		}
		if config.ConnectTo == "" {
			config.ConnectTo = DefaultTincRemotePeers
		}
		if config.Device == "" {
			config.Device = tincDeviceTunTap
		}
		if config.DeviceType == "" {
			config.DeviceType = tincDeviceTypeTap
		}
		if config.Mode == "" {
			config.Mode = tincModeSwitch
		}
		if config.Name == "" {
			config.Name = tincMainName
		}
		if config.CPU == "" {
			config.CPU = defaultCPUCapacity
		}
		if config.Memory == "" {
			config.Memory = defaultMemoryCapacity
		}
		if config.Pods == "" {
			config.Pods = defaultPodCapacity
		}
	}

	if _, err = resource.ParseQuantity(config.CPU); err != nil {
		return config, fmt.Errorf("Invalid CPU value %v", config.CPU)
	}
	if _, err = resource.ParseQuantity(config.Memory); err != nil {
		return config, fmt.Errorf("Invalid memory value %v", config.Memory)
	}
	if _, err = resource.ParseQuantity(config.Pods); err != nil {
		return config, fmt.Errorf("Invalid pods value %v", config.Pods)
	}
	return config, nil
}

// CreatePod accepts a Pod definition and stores it in memory.
func (p *TincProvider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	log.Printf("receive CreatePod %q\n", pod.Name)

	key, err := buildKey(pod)
	if err != nil {
		return err
	}

	p.pods[key] = pod

	if vpnMode := pod.Annotations["vpnmode"]; vpnMode == "peer" {
		privateAddress = DefaultTincPeerPrivateAddress
		tincMainName = DefaultTincRemotePeers
		tincPeerName = DefaultTincMainName
	} else {
		privateAddress = DefaultTincMainPrivateAddress
		tincMainName = DefaultTincMainName
		tincPeerName = DefaultTincRemotePeers
	}

	tincStartupConfigHost = filepath.Join("/tmp", tincMainName, "vk-startup-config.conf")
	tincMainConfigHost = filepath.Join("/tmp", tincMainName, "vk-main.conf")
	tincUpScriptHost = filepath.Join("/tmp", tincMainName, "vk-tinc-up")

	if err := p.createStartupConfig(); err != nil {
		return err
	}

	_, _ = exec.Command(dockerClient, "rm", "--force", tincMainName).Output()

	out, err := exec.Command(dockerClient, "run", "--privileged", "--name="+tincMainName,
		"--detach", "--rm",
		fmt.Sprintf("--volume=%s:%s", tincStartupConfigHost, tincStartupConfigContainer),
		fmt.Sprintf("--volume=%s:%s", tincMainConfigHost, tincMainConfigContainer),
		fmt.Sprintf("--volume=%s:%s", tincUpScriptHost, tincUpScriptContainer),
		tincImageName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run docker-run:\nout: %s\nerr: %v\n", string(out), err)
	}

	return nil
}

// UpdatePod accepts a Pod definition and updates its reference.
func (p *TincProvider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	log.Printf("receive UpdatePod %q\n", pod.Name)

	key, err := buildKey(pod)
	if err != nil {
		return err
	}

	p.pods[key] = pod

	return nil
}

// DeletePod deletes the specified pod out of memory.
func (p *TincProvider) DeletePod(ctx context.Context, pod *v1.Pod) (err error) {
	log.Printf("receive DeletePod %q\n", pod.Name)

	key, err := buildKey(pod)
	if err != nil {
		return err
	}

	if _, exists := p.pods[key]; !exists {
		return strongerrors.NotFound(fmt.Errorf("pod not found"))
	}

	delete(p.pods, key)

	fmt.Printf("running docker-rm %s\n", tincMainName)

	out, err := exec.Command(dockerClient, "rm", "--force", p.config.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run docker-rm:\nout: %s\nerr: %v\n", string(out), err)
	}

	return nil
}

// GetPod returns a pod by name that is stored in memory.
func (p *TincProvider) GetPod(ctx context.Context, namespace, name string) (pod *v1.Pod, err error) {
	log.Printf("receive GetPod %q\n", name)

	key, err := buildKeyFromNames(namespace, name)
	if err != nil {
		return nil, err
	}

	if pod, ok := p.pods[key]; ok {
		return pod, nil
	}
	return nil, strongerrors.NotFound(fmt.Errorf("pod \"%s/%s\" is not known to the provider", namespace, name))
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *TincProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, tail int) (string, error) {
	log.Printf("receive GetContainerLogs %q\n", podName)
	return "", nil
}

// GetPodFullName is full pod name as defined in the provider context
func (p *TincProvider) GetPodFullName(namespace string, pod string) string {
	return ""
}

// ExecInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *TincProvider) ExecInContainer(name string, uid types.UID, container string, cmd []string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, timeout time.Duration) error {
	log.Printf("receive ExecInContainer %q\n", container)
	return nil
}

// GetPodStatus returns the status of a pod by name that is "running".
// returns nil if a pod by that name is not found.
func (p *TincProvider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	log.Printf("receive GetPodStatus %q\n", name)

	now := metav1.NewTime(time.Now())

	status := &v1.PodStatus{
		Phase:     v1.PodRunning,
		HostIP:    "1.2.3.4",
		PodIP:     "5.6.7.8",
		StartTime: &now,
		Conditions: []v1.PodCondition{
			{
				Type:   v1.PodInitialized,
				Status: v1.ConditionTrue,
			},
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
			{
				Type:   v1.PodScheduled,
				Status: v1.ConditionTrue,
			},
		},
	}

	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	for _, container := range pod.Spec.Containers {
		status.ContainerStatuses = append(status.ContainerStatuses, v1.ContainerStatus{
			Name:         container.Name,
			Image:        container.Image,
			Ready:        true,
			RestartCount: 0,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{
					StartedAt: now,
				},
			},
		})
	}

	return status, nil
}

// GetPods returns a list of all pods known to be "running".
func (p *TincProvider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	log.Printf("receive GetPods\n")

	var pods []*v1.Pod

	for _, pod := range p.pods {
		pods = append(pods, pod)
	}

	return pods, nil
}

// Capacity returns a resource list containing the capacity limits.
// It must contain "pods" at least, so that the provider gets registered.
func (p *TincProvider) Capacity(ctx context.Context) v1.ResourceList {
	return v1.ResourceList{
		"cpu":    resource.MustParse(p.config.CPU),
		"memory": resource.MustParse(p.config.Memory),
		"pods":   resource.MustParse(p.config.Pods),
	}
}

// NodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status
// within Kubernetes.
func (p *TincProvider) NodeConditions(ctx context.Context) []v1.NodeCondition {
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}

}

// NodeAddresses returns a list of addresses for the node status
// within Kubernetes.
func (p *TincProvider) NodeAddresses(ctx context.Context) []v1.NodeAddress {
	return []v1.NodeAddress{
		{
			Type:    "InternalIP",
			Address: p.tincAddress,
		},
	}
}

// NodeDaemonEndpoints returns NodeDaemonEndpoints for the node status
// within Kubernetes.
func (p *TincProvider) NodeDaemonEndpoints(ctx context.Context) *v1.NodeDaemonEndpoints {
	return &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.tincPort,
		},
	}
}

// OperatingSystem returns the operating system for this provider.
// This is a noop to default to Linux for now.
func (p *TincProvider) OperatingSystem() string {
	return providers.OperatingSystemLinux
}

// GetStatsSummary returns dummy stats for all pods known by this provider.
func (p *TincProvider) GetStatsSummary(ctx context.Context) (*stats.Summary, error) {
	// Return the dummy stats.
	return &stats.Summary{}, nil
}

// createStartupConfig accepts a Pod definition and stores it in memory.
func (p *TincProvider) createStartupConfig() error {
	// /tmp/.../vk-startup-config.conf
	data := fmt.Sprintf("add AutoConnect = %s\n", p.config.AutoConnect)
	data += fmt.Sprintf("add ConnectTo = %s\n", tincPeerName)
	data += fmt.Sprintf("add Device = %s\n", p.config.Device)
	data += fmt.Sprintf("add DeviceType = %s\n", p.config.DeviceType)
	data += fmt.Sprintf("add Mode = %s\n", p.config.Mode)
	data += fmt.Sprintf("add Name = %s\n", tincMainName)

	nodeMain := p.config.Name

	data += fmt.Sprintf("add %s.Address = %s\n", nodeMain, myAddress)
	data += fmt.Sprintf("add %s.Subnet = %s\n", nodeMain, p.tincSubnet)
	data += fmt.Sprintf("add %s.Port = %d\n", nodeMain, p.tincPort)

	if err := os.MkdirAll(filepath.Join("/tmp", tincMainName), os.FileMode(0775)); err != nil {
		return err
	}

	nodePeers := strings.Fields(DefaultTincRemotePeers)

	for _, nodePeer := range nodePeers {
		data += fmt.Sprintf("add %s.Address = %s\n", nodePeer, peerAddress)
		data += fmt.Sprintf("add %s.Subnet = %s\n", nodePeer, p.tincSubnet)
		data += fmt.Sprintf("add %s.Port = %d\n", nodePeer, p.tincPort)
	}

	if err := ioutil.WriteFile(tincStartupConfigHost, []byte(data), os.FileMode(0644)); err != nil {
		return err
	}

	// /tmp/.../vk-main.conf
	dataMain := fmt.Sprintf("AutoConnect = %s\n", p.config.AutoConnect)
	dataMain += fmt.Sprintf("ConnectTo = %s\n", tincPeerName)
	dataMain += fmt.Sprintf("Device = %s\n", p.config.Device)
	dataMain += fmt.Sprintf("DeviceType = %s\n", p.config.DeviceType)
	dataMain += fmt.Sprintf("ExperimentalProtocol = %s\n", p.config.ExperimentalProtocol)
	dataMain += fmt.Sprintf("Mode = %s\n", p.config.Mode)
	dataMain += fmt.Sprintf("Name = %s\n", tincMainName)

	if err := ioutil.WriteFile(tincMainConfigHost, []byte(dataMain), os.FileMode(0644)); err != nil {
		return err
	}

	// /tmp/.../vk-tinc-up

	dataScr := fmt.Sprintf("#!/bin/bash\n")
	dataScr += fmt.Sprintf("ip link set tap0 up\n")
	dataScr += fmt.Sprintf("ip addr add %s/24 dev tap0\n", privateAddress)

	if err := ioutil.WriteFile(tincUpScriptHost, []byte(dataScr), os.FileMode(0755)); err != nil {
		return err
	}

	return nil
}

func buildKeyFromNames(namespace string, name string) (string, error) {
	return fmt.Sprintf("%s-%s", namespace, name), nil
}

// buildKey is a helper for building the "key" for the providers pod store.
func buildKey(pod *v1.Pod) (string, error) {
	if pod.ObjectMeta.Namespace == "" {
		return "", fmt.Errorf("pod namespace not found")
	}

	if pod.ObjectMeta.Name == "" {
		return "", fmt.Errorf("pod name not found")
	}

	return buildKeyFromNames(pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)
}
