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
	"strings"
	"time"

	"github.com/cpuguy83/strongerrors"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
)

const (
	tincAutoConnect string = "yes"

	tincModeRouter string = "router"
	tincModeSwitch string = "switch"

	tincDeviceTunTap string = "/dev/net/tun"

	tincDeviceTypeDummy string = "dummy"
	tincDeviceTypeTap   string = "tap"

	tincContainerName string = "nodemain"
	tincImageName     string = "dongsupark/tinc"

	tincStartupConfigHost      string = "/tmp/vk-startup-config.conf"
	tincStartupConfigContainer string = "/environment/default.startup.conf"

	dockerClient = "/usr/bin/docker"

	// DefaultTincRemotePeers is a list of remote hostnames to be connected.
	// Space-separated: e.g. "nodepeer1 nodepeer2"
	DefaultTincRemotePeers string = "nodepeer"

	// DefaultTincSubnet is a subnetwork for the test nodes
	DefaultTincSubnet string = "10.1.1.0/24"

	// DefaultTincPort is the default port number Tinc VPN listens on
	DefaultTincPort int32 = 655
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
			config.Name = tincContainerName
		}
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

	if err := p.createStartupConfig(); err != nil {
		return err
	}

	if _, err := exec.Command(dockerClient, "run", "--privileged", "--name="+p.config.Name, "-ti", "--rm",
		fmt.Sprintf("--volume=%s:%s", tincStartupConfigHost, tincStartupConfigContainer),
		tincImageName).Output(); err != nil {
		return err
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

	if _, err := exec.Command(dockerClient, "rm", p.config.Name).Output(); err != nil {
		return err
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
func (p *TincProvider) Capacity(ctx context.Context) v1.ResourceList {
	return v1.ResourceList{}
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
	return ""
}

// GetStatsSummary returns dummy stats for all pods known by this provider.
func (p *TincProvider) GetStatsSummary(ctx context.Context) (*stats.Summary, error) {
	// Return the dummy stats.
	return &stats.Summary{}, nil
}

// createStartupConfig accepts a Pod definition and stores it in memory.
func (p *TincProvider) createStartupConfig() error {
	data := fmt.Sprintf("add AutoConnect = %s\n", p.config.AutoConnect)
	data += fmt.Sprintf("add ConnectTo = %s\n", p.config.ConnectTo)
	data += fmt.Sprintf("add Device = %s\n", p.config.Device)
	data += fmt.Sprintf("add DeviceType = %s\n", p.config.DeviceType)
	data += fmt.Sprintf("add Mode = %s\n", p.config.Mode)
	data += fmt.Sprintf("add Name = %s\n", p.config.Name)

	nodeMain := p.config.Name

	data += fmt.Sprintf("add %s.Address = %s\n", nodeMain, p.tincAddress)
	data += fmt.Sprintf("add %s.Subnet = %s\n", nodeMain, p.tincSubnet)
	data += fmt.Sprintf("add %s.Port = %s\n", nodeMain, p.tincPort)

	nodePeers := strings.Fields(DefaultTincRemotePeers)

	for _, nodePeer := range nodePeers {
		data += fmt.Sprintf("add %s.Address = %s\n", nodePeer, p.tincAddress)
		data += fmt.Sprintf("add %s.Subnet = %s\n", nodePeer, p.tincSubnet)
		data += fmt.Sprintf("add %s.Port = %s\n", nodePeer, p.tincPort)
	}

	if err := ioutil.WriteFile(tincStartupConfigHost, []byte(data), os.FileMode(0644)); err != nil {
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
