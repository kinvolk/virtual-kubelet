# Running virtual kubelet with tinc provider


You can run virtual-kubelet with tinc provider like below.
It's assumed that the k8s cluster was configured locally via minikube.

```
VK_SRC=$GOPATH/src/github.com/virtual-kubelet/virtual-kubelet
VK_BINPATH=$VK_SRC/bin/virtual-kubelet

VKUBELET_POD_IP=$(minikube ip) \
APISERVER_CERT_LOCATION=$VK_SRC/hack/tinc/vkubelet-tinc-0-crt.pem \
APISERVER_KEY_LOCATION=$VK_SRC/hack/tinc/vkubelet-tinc-0-key.pem \
$VK_BINPATH --provider=tinc --provider-config=$VK_SRC/hack/tinc/vk-tinc.json \
--kubeconfig=$KUBECONFIG --namespace=default --nodename=vkubelet-tinc-0
```

