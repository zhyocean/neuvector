package cache

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	"github.com/zhyocean/neuvector/controller/access"
	"github.com/zhyocean/neuvector/controller/api"
	"github.com/zhyocean/neuvector/controller/common"
	"github.com/zhyocean/neuvector/controller/scan"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/cluster"
	"github.com/zhyocean/neuvector/share/utils"
)

type webhookCache struct {
	conn   *common.Webhook
	target string
}

var systemConfigCache share.CLUSSystemConfig = common.DefaultSystemConfig
var fedSystemConfigCache share.CLUSSystemConfig = share.CLUSSystemConfig{CfgType: share.FederalCfg}
var webhookCacheMap map[string]*webhookCache = make(map[string]*webhookCache, 0)    // Only the enabled webhooks
var fedWebhookCacheMap map[string]*webhookCache = make(map[string]*webhookCache, 0) // Only the enabled webhooks
var syslogger *common.Syslogger

func workloadConfig(nType cluster.ClusterNotifyType, key string, value []byte) {
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		id := share.CLUSUniconfKey2ID(key)

		var cconf share.CLUSWorkloadConfig
		json.Unmarshal(value, &cconf)

		cacheMutexLock()
		if cache, ok := wlCacheMap[id]; ok {
			cache.config = &cconf
		}
		cacheMutexUnlock()

		log.WithFields(log.Fields{"config": cconf, "id": id}).Debug("")
	}
}

func agentConfig(nType cluster.ClusterNotifyType, key string, value []byte) {
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		id := share.CLUSUniconfKey2ID(key)

		var cconf share.CLUSAgentConfig
		json.Unmarshal(value, &cconf)

		cacheMutexLock()
		if cache, ok := agentCacheMap[id]; ok {
			cache.config = &cconf
		}
		cacheMutexUnlock()

		log.WithFields(log.Fields{"config": cconf, "id": id}).Debug("")
	}
}

func setControllerDebug(debug []string, debugCPath bool) {
	var hasCPath, hasConn, hasMutex, hasScan, hasCluster bool

	for _, d := range debug {
		switch d {
		case "cpath":
			hasCPath = true
		case "mutex":
			hasMutex = true
		case "conn":
			hasConn = true
		case "scan":
			hasScan = true
		case "cluster":
			hasCluster = true
		}
	}
	if debugCPath || hasCPath {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	if hasConn {
		cctx.ConnLog.Level = log.DebugLevel
	} else {
		cctx.ConnLog.Level = log.InfoLevel
	}
	if hasScan || debugCPath || hasCPath {
		cctx.ScanLog.Level = log.DebugLevel
	} else {
		cctx.ScanLog.Level = log.InfoLevel
	}
	if hasMutex {
		cctx.MutexLog.Level = log.DebugLevel
	} else {
		cctx.MutexLog.Level = log.InfoLevel
	}
	if hasCluster || debugCPath {
		cluster.SetLogLevel(log.DebugLevel)
	} else {
		cluster.SetLogLevel(log.InfoLevel)
	}
}

func controllerConfig(nType cluster.ClusterNotifyType, key string, value []byte) {
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		id := share.CLUSUniconfKey2ID(key)

		var cconf share.CLUSControllerConfig
		json.Unmarshal(value, &cconf)

		cacheMutexLock()
		if cache, ok := ctrlCacheMap[id]; ok {
			cache.config = &cconf
		} else {
			ctrlCacheMap[id] = initCtrlCache(id)
			ctrlCacheMap[id].config = &cconf
		}
		cacheMutexUnlock()

		if id == localDev.Ctrler.ID {
			setControllerDebug(cconf.Debug, false)
		}

		log.WithFields(log.Fields{"config": cconf, "id": id}).Debug("")
	}
}

func uniconfUpdate(nType cluster.ClusterNotifyType, key string, value []byte) {
	log.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")

	subject := share.CLUSUniconfKey2Subject(key)
	switch subject {
	case "workload":
		workloadConfig(nType, key, value)
	case "agent":
		agentConfig(nType, key, value)
	case "controller":
		controllerConfig(nType, key, value)
	}
}

func uniconfWorkloadDelete(id string, param interface{}) {
	if isLeader() == false {
		return
	}
	cache := param.(*workloadCache)
	hostID := cache.workload.HostID
	key := share.CLUSUniconfWorkloadKey(hostID, id)
	cluster.Delete(key)
}

func uniconfAgentDelete(id string, param interface{}) {
	if isLeader() == false {
		return
	}
	agent := param.(*agentCache).agent
	key := share.CLUSUniconfAgentKey(agent.HostID, id)
	cluster.Delete(key)
}

func uniconfControllerDelete(id string, param interface{}) {
	if isLeader() == false {
		return
	}
	key := share.CLUSUniconfControllerKey(id, id)
	cluster.Delete(key)
}

func getNewServicePolicyMode() string {
	return systemConfigCache.NewServicePolicyMode
}

func getNewServiceProfileBaseline() string {
	return systemConfigCache.NewServiceProfileBaseline
}

func (m CacheMethod) GetNewServicePolicyMode() string {
	return getNewServicePolicyMode()
}

func (m CacheMethod) GetNewServiceProfileBaseline() string {
	return getNewServiceProfileBaseline()
}

func getUnusedGroupAging() uint8 {
	return systemConfigCache.UnusedGroupAging
}

func (m CacheMethod) GetUnusedGroupAging() uint8 {
	return getUnusedGroupAging()
}

func (m CacheMethod) GetSystemConfig(acc *access.AccessControl) *api.RESTSystemConfig {
	if !acc.Authorize(&systemConfigCache, nil) {
		return nil
	}

	//cacheMutexRLock() //-> TO CHECK
	//defer cacheMutexRUnlock()

	rconf := api.RESTSystemConfig{
		NewServicePolicyMode:      systemConfigCache.NewServicePolicyMode,
		NewServiceProfileBaseline: systemConfigCache.NewServiceProfileBaseline,
		UnusedGroupAging:          systemConfigCache.UnusedGroupAging,
		SyslogLevel:               systemConfigCache.SyslogLevel,
		SyslogIPProto:             systemConfigCache.SyslogIPProto,
		SyslogPort:                systemConfigCache.SyslogPort,
		SyslogEnable:              systemConfigCache.SyslogEnable,
		SyslogCategories:          systemConfigCache.SyslogCategories,
		SyslogInJSON:              systemConfigCache.SyslogInJSON,
		SingleCVEPerSyslog:        systemConfigCache.SingleCVEPerSyslog,
		AuthOrder:                 systemConfigCache.AuthOrder,
		AuthByPlatform:            systemConfigCache.AuthByPlatform,
		InternalSubnets:           systemConfigCache.InternalSubnets,
		ClusterName:               systemConfigCache.ClusterName,
		ControllerDebug:           systemConfigCache.ControllerDebug,
		MonitorServiceMesh:        systemConfigCache.TapProxymesh,
		IBMSAEpEnabled:            systemConfigCache.IBMSAConfigNV.EpEnabled,
		IBMSAEpStart:              systemConfigCache.IBMSAConfigNV.EpStart,
		IBMSAEpDashboardURL:       systemConfigCache.IBMSAConfigNV.EpDashboardURL,
		IBMSAEpConnectedAt:        api.RESTTimeString(systemConfigCache.IBMSAConfigNV.EpConnectedAt),
		XffEnabled:                systemConfigCache.XffEnabled,
	}
	if systemConfigCache.SyslogIP != nil {
		rconf.SyslogServer = systemConfigCache.SyslogIP.String()
	} else {
		rconf.SyslogServer = systemConfigCache.SyslogServer
	}
	if systemConfigCache.SyslogIPProto == 0 {
		rconf.SyslogIPProto = 17
	}

	rconf.Webhooks = make([]api.RESTWebhook, len(systemConfigCache.Webhooks))
	for i, wh := range systemConfigCache.Webhooks {
		rconf.Webhooks[i] = api.RESTWebhook{Name: wh.Name, Url: wh.Url, Enable: wh.Enable, Type: wh.Type, CfgType: api.CfgTypeUserCreated}
	}

	proxy := systemConfigCache.RegistryHttpProxy
	rconf.RegistryHttpProxyEnable = proxy.Enable
	rconf.RegistryHttpProxy = api.RESTProxy{
		URL:      proxy.URL,
		Username: proxy.Username,
		Password: proxy.Password,
	}
	proxy = systemConfigCache.RegistryHttpsProxy
	rconf.RegistryHttpsProxyEnable = proxy.Enable
	rconf.RegistryHttpsProxy = api.RESTProxy{
		URL:      proxy.URL,
		Username: proxy.Username,
		Password: proxy.Password,
	}

	return &rconf
}

func (m CacheMethod) GetFedSystemConfig(acc *access.AccessControl) *share.CLUSSystemConfig {
	if !acc.Authorize(&fedSystemConfigCache, nil) {
		return nil
	}

	cacheMutexRLock()
	defer cacheMutexRUnlock()

	rconf := share.CLUSSystemConfig{
		Webhooks: make([]share.CLUSWebhook, len(fedSystemConfigCache.Webhooks)),
	}
	for i, wh := range fedSystemConfigCache.Webhooks {
		rconf.Webhooks[i] = wh
	}

	return &rconf
}

func (m CacheMethod) GetIBMSAConfig(acc *access.AccessControl) (*api.RESTIBMSAConfig, error) {
	if !acc.Authorize(&systemConfigCache, nil) {
		return nil, common.ErrObjectAccessDenied
	}

	//cacheMutexRLock() //-> TO CHECK
	//defer cacheMutexRUnlock()

	rconf := &api.RESTIBMSAConfig{
		AccountID:         systemConfigCache.IBMSAConfig.AccountID,
		APIKey:            systemConfigCache.IBMSAConfig.APIKey,
		ProviderID:        systemConfigCache.IBMSAConfig.ProviderID,
		FindingsURL:       systemConfigCache.IBMSAConfig.FindingsURL,
		TokenURL:          systemConfigCache.IBMSAConfig.TokenURL,
		OnboardNoteName:   systemConfigCache.IBMSAOnboardData.NoteName,
		OnboardID:         systemConfigCache.IBMSAOnboardData.ID,
		OnboardProviderID: systemConfigCache.IBMSAOnboardData.ProviderID,
	}

	return rconf, nil
}

func (m CacheMethod) GetIBMSAConfigNV(acc *access.AccessControl) (share.CLUSIBMSAConfigNV, error) {
	if !acc.Authorize(&systemConfigCache, nil) {
		return share.CLUSIBMSAConfigNV{}, common.ErrObjectAccessDenied
	}

	//cacheMutexRLock() //-> TO CHECK
	//defer cacheMutexRUnlock()

	return systemConfigCache.IBMSAConfigNV, nil
}

func (m CacheMethod) GetSystemConfigClusterName(acc *access.AccessControl) string {
	if !acc.Authorize(&systemConfigCache, nil) {
		return ""
	}

	//cacheMutexRLock() //-> TO CHECK
	//defer cacheMutexRUnlock()

	return systemConfigCache.ClusterName
}

func systemConfigUpdate(nType cluster.ClusterNotifyType, key string, value []byte) {
	log.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")

	var cfg share.CLUSSystemConfig

	// cacheMutexLock() //-> TO CHECK

	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		json.Unmarshal(value, &cfg)
		log.WithFields(log.Fields{"config": cfg}).Debug("")

		if cfg.IBMSAConfigNV.EpEnabled && cfg.IBMSAConfigNV.EpStart == 1 {
			if isLeader() {
				var param interface{} = &cfg.IBMSAConfig
				cctx.StartStopFedPingPollFunc(share.StartPostToIBMSA, 0, param)
			}
		} else {
			// customer explicitly disables IBM SA endpoint
			cctx.StartStopFedPingPollFunc(share.StopPostToIBMSA, 0, nil)
		}

	case cluster.ClusterNotifyDelete:
		// Triggered at configuration import
		cfg = common.DefaultSystemConfig
	}

	// Only apply debug flags if they change so we can apply the initial debugCPath flag correctly.
	if !utils.CompareSliceWithoutOrder(systemConfigCache.ControllerDebug, cfg.ControllerDebug) {
		setControllerDebug(cfg.ControllerDebug, false)
	}

	if systemConfigCache.RegistryHttpProxy != cfg.RegistryHttpProxy ||
		systemConfigCache.RegistryHttpsProxy != cfg.RegistryHttpsProxy {
		scan.UpdateProxy(&cfg.RegistryHttpProxy, &cfg.RegistryHttpsProxy)
	}

	systemConfigCache = cfg

	fedCacheMutexLock()
	fedHttpsProxyCache = systemConfigCache.RegistryHttpsProxy
	fedHttpProxyCache = systemConfigCache.RegistryHttpProxy
	fedCacheMutexUnlock()
	//cacheMutexUnlock() //-> TO CHECK

	putInternalIPNetToCluseter(true)

	webhookCachTemp := make(map[string]*webhookCache, 0)
	for _, h := range systemConfigCache.Webhooks {
		if h.Enable {
			webhookCachTemp[h.Name] = &webhookCache{conn: common.NewWebHook(h.Url), target: h.Type}
		}
	}
	webhookCacheMap = webhookCachTemp

	syslogMutexLock()
	defer syslogMutexUnlock()

	if systemConfigCache.SyslogEnable {
		syslogger = common.NewSyslogger(&systemConfigCache.CLUSSyslogConfig)
	} else if syslogger != nil {
		syslogger.Close()
		syslogger = nil
	}
}

func configInit() {
	acc := access.NewReaderAccessControl()
	cfg, _ := clusHelper.GetSystemConfigRev(acc)
	systemConfigCache = *cfg
	if cfg.IBMSAConfigNV.EpEnabled && cfg.IBMSAConfigNV.EpStart == 1 {
		var param interface{} = &cfg.IBMSAConfig
		cctx.StartStopFedPingPollFunc(share.StartPostToIBMSA, 0, param)
	}
	setControllerDebug(systemConfigCache.ControllerDebug, cctx.DebugCPath)
	scan.UpdateProxy(&systemConfigCache.RegistryHttpProxy, &systemConfigCache.RegistryHttpsProxy)

	// uniconf key deleted when controller exits, no need to recover it.
}
