// +build !no_tinc_provider

package register

import (
	"github.com/virtual-kubelet/virtual-kubelet/providers"
	"github.com/virtual-kubelet/virtual-kubelet/providers/tinc"
)

func init() {
	register("tinc", initTinc)
}

func initTinc(cfg InitConfig) (providers.Provider, error) {
	return tinc.NewTincProvider(
		cfg.ConfigPath,
		cfg.NodeName,
		cfg.InternalIP,
		tinc.DefaultTincSubnet,
		tinc.DefaultTincPort,
	)
}
