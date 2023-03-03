package main

import (
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/global"
	"github.com/zhyocean/neuvector/share/system"
	sk "github.com/zhyocean/neuvector/share/system/sidekick"
	"github.com/zhyocean/neuvector/share/utils"
)

func getHostAddrs() map[string]sk.NetIface {
	path := fmt.Sprintf("%s --act ports", system.ExecSidekick)
	value, err := global.SYS.NsRunBinary(1, path)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Error getting host IP")
		return make(map[string]sk.NetIface)
	}

	var ifaces map[string]sk.NetIface
	json.Unmarshal(value, &ifaces)
	return ifaces
}

/*
With Azure advanced networking plugin:
 link - link=eth0 type=device
 link - link=docker0 type=bridge
 Switch - ipnet={IP:172.17.0.1 Mask:ffff0000} link=docker0
 link - link=enP1p0s2 type=device
 link - link=azure0 type=bridge
 Switch - ipnet={IP:10.240.0.35 Mask:ffff0000} link=azure0
 link - link=azv1769de20eea type=veth
 link - link=lo type=device

 2: eth0: <BROADCAST,UP,LOWER_UP> mtu 1500 qdisc mq master azure0 state UP qlen 1000
 5: azure0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP qlen 1000
*/

func parseHostAddrs(ifaces map[string]sk.NetIface, platform, network string) (map[string][]share.CLUSIPAddr, utils.Set, bool) {
	devs := make(map[string][]share.CLUSIPAddr)
	ips := utils.NewSet()
	maxMTU := 0

	for name, iface := range ifaces {
		log.WithFields(log.Fields{"link": name, "type": iface.Type, "mtu": iface.Mtu, "flags": iface.Flags}).Info("link")

		if iface.Mtu <= share.NV_VBR_PORT_MTU_JUMBO && maxMTU < iface.Mtu {
			maxMTU = iface.Mtu
		}
		if iface.Type == "device" || iface.Type == "bond" || iface.Type == "vlan" {
			for _, addr := range iface.Addrs {
				if utils.IsIPv4(addr.IPNet.IP) {
					log.WithFields(log.Fields{"link": name, "ipnet": addr.IPNet}).Info("Global")
					devs[name] = append(devs[name], share.CLUSIPAddr{
						IPNet: addr.IPNet,
						Scope: share.CLUSIPAddrScopeNAT,
					})
					ips.Add(addr.IPNet.IP.String())
				}
			}
		} else if iface.Type == "bridge" {
			if platform == share.PlatformKubernetes && strings.HasPrefix(name, "cni") {
				continue
			}

			for _, addr := range iface.Addrs {
				if utils.IsIPv4(addr.IPNet.IP) {
					log.WithFields(log.Fields{"link": name, "ipnet": addr.IPNet}).Info("Switch")
					if name == "azure0" {
						devs[name] = append(devs[name], share.CLUSIPAddr{
							IPNet: addr.IPNet,
							Scope: share.CLUSIPAddrScopeNAT,
						})
					}
					ips.Add(addr.IPNet.IP.String())
				}
			}
		}
	}
	log.WithFields(log.Fields{"maxMTU": maxMTU}).Info("")
	if maxMTU > share.NV_VBR_PORT_MTU { //jumbo frame mtu
		return devs, ips, true
	} else {
		return devs, ips, false
	}
}
