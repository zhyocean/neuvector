package main

// #include "../defs.h"
import "C"

import (
	"net"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/agent/dp"
	"github.com/zhyocean/neuvector/agent/probe"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/cluster"
	"github.com/zhyocean/neuvector/share/fsmon"
	"github.com/zhyocean/neuvector/share/utils"
)

type eventPolicyHandlerFunc func(arg interface{})

func isContainerBlocking(c *containerData) bool {
	if !c.capBlock {
		return false
	}
	if c.policyMode == "" {
		c.policyMode = gInfo.policyMode
	}

	return c.policyMode == share.PolicyModeEnforce
}

func isContainerInline(c *containerData) bool {
	if !c.capIntcp {
		return false
	}
	if c.policyMode == "" {
		c.policyMode = gInfo.policyMode
	}

	if c.policyMode == share.PolicyModeEnforce {
		return true
	} else if cfg, ok := gInfo.containerConfig[c.id]; ok {
		return cfg.Wire == share.WireInline
	} else {
		return false
	}
}

func isContainerQuarantine(c *containerData) bool {
	if !c.capIntcp {
		return false
	}
	if cfg, ok := gInfo.containerConfig[c.id]; ok {
		return cfg.Quarantine
	} else {
		return false
	}
}

// Here we compare the difference between the new config from cluster and local cache.
// In the case that the agent process restarts, we need to make sure all local states should
// be consistent with the local cache.
func taskConfigContainer(id string, newconf *share.CLUSWorkloadConfig) {
	log.WithFields(log.Fields{"container": id, "config": *newconf}).Debug("")
	gInfo.containerConfig[id] = newconf
	if c, ok := gInfo.activeContainers[id]; ok && c.capIntcp {
		inline := isContainerInline(c)
		quar := isContainerQuarantine(c)
		if inline != c.inline || quar != c.quar {
			changeContainerWire(c, inline, quar, &newconf.QuarReason)
		}
	}
}

func taskAppUpdateByMAC(mac net.HardwareAddr, apps map[share.CLUSProtoPort]*share.CLUSApp) {
	if c := getContainerByMAC(mac); c != nil {
		mergeContainerApps(c, apps)
	}
}

func mergeContainerApps(c *containerData, apps map[share.CLUSProtoPort]*share.CLUSApp) {
	log.WithFields(log.Fields{"container": c.id}).Debug("")

	gInfoLock()
	for p, app := range apps {
		if m, ok := c.appMap[p]; ok {
			if app.Proto > 0 {
				m.Proto = app.Proto
			}
			if app.Server > 0 {
				m.Server = app.Server
			}
			if app.Application > 0 {
				m.Application = app.Application
			}
		} else {
			c.appMap[p] = app
		}
	}
	gInfoUnlock()

	ev := ClusterEvent{
		event: EV_UPDATE_CONTAINER,
		id:    c.id,
		apps:  translateAppMap(c.appMap),
	}
	ClusterEventChan <- &ev
}

func mergeMappedPorts(c *containerData, ports map[share.CLUSProtoPort]*share.CLUSMappedPort) {
	log.WithFields(log.Fields{"merge-to": c.id}).Debug("")

	gInfoLock()
	for p, port := range ports {
		c.portMap[p] = port
	}
	gInfoUnlock()

	ev := ClusterEvent{
		event: EV_UPDATE_CONTAINER,
		id:    c.id,
		ports: translateMappedPort(c.portMap),
	}
	ClusterEventChan <- &ev
}

func configKvCongestCtl(enable bool) {
	if agentEnv.kvCongestCtrl == enable {
		log.Debug("Skipped")
		return
	}

	agentEnv.kvCongestCtrl = enable
	log.WithFields(log.Fields{"enable": agentEnv.kvCongestCtrl}).Info()
	go cluster.SetWatcherCongestionCtl(share.CLUSWorkloadProfileStore, agentEnv.kvCongestCtrl)
}

func taskConfigAgent(conf *share.CLUSAgentConfig) {
	log.WithFields(log.Fields{"config": conf}).Debug("")

	// debug
	var hasCPath, hasConn, hasCluster, hasMonitorTrace bool
	if conf.Debug == nil {
		conf.Debug = make([]string, 0)
	}
	newDebug := utils.NewSet()
	for _, d := range conf.Debug {
		switch d {
		case "cpath":
			hasCPath = true
			newDebug.Add("ctrl")
		case "conn":
			hasConn = true
		case "cluster":
			hasCluster = true
		case "monitor":
			hasMonitorTrace = true
		default:
			newDebug.Add(d)
		}
	}
	if hasCPath {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	if hasConn {
		connLog.Level = log.DebugLevel
	} else {
		connLog.Level = log.InfoLevel
	}

	prober.SetMonitorTrace(hasMonitorTrace)
	fileWatcher.SetMonitorTrace(hasMonitorTrace)

	if !agentEnv.runWithController {
		if hasCluster {
			cluster.SetLogLevel(log.DebugLevel)
		} else {
			cluster.SetLogLevel(log.InfoLevel)
		}
	}

	oldDebug := utils.NewSet()
	for _, d := range gInfo.agentConfig.Debug {
		oldDebug.Add(d)
	}
	if !oldDebug.Equal(newDebug) {
		// rebuild debug config because we might add 'ctrl'
		i := 0
		cats := make([]string, newDebug.Cardinality())
		for d := range newDebug.Iter() {
			cats[i] = d.(string)
			i++
		}

		gInfo.agentConfig.Debug = cats

		debug := &dp.DPDebug{Categories: cats}
		dp.DPCtrlConfigAgent(debug)
	}

	//////
	prober.SetNvProtect(conf.DisableNvProtectMode) // default: false (enabled)
	configKvCongestCtl(!conf.DisableKvCongestCtl)
}

func escalToIncidentLog(e *probe.ProbeEscalation, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := share.CLUSIncidentLog{
		HostID:       Host.ID,
		HostName:     Host.Name,
		AgentID:      Agent.ID,
		AgentName:    Agent.Name,
		WorkloadID:   e.ID,
		ReportedAt:   time.Now().UTC(),
		ProcName:     e.Name,
		ProcPath:     e.Path,
		ProcCmds:     e.Cmds,
		ProcRealUID:  e.RUid,
		ProcEffUID:   e.EUid,
		ProcRealUser: e.RealUser,
		ProcEffUser:  e.EffUser,

		// parent info
		ProcPName: e.ParentName,
		ProcPPath: e.ParentPath,

		Action:  share.PolicyActionViolate,
		RuleID:  share.CLUSReservedUuidRootEscalation,
		Msg:     e.Msg,
		Count:   count,
		StartAt: start,
	}

	gInfoRLock()
	defer gInfoRUnlock()

	if e.ID == "" {
		//host privilege escalation
		eLog.ID = share.CLUSIncidHostPrivilEscalate
	} else if c, ok := gInfo.activeContainers[e.ID]; ok {
		eLog.WorkloadName = c.name
		eLog.ID = share.CLUSIncidContainerPrivilEscalate
	} else {
		eLog.WorkloadName = ""
		eLog.ID = share.CLUSIncidContainerPrivilEscalate
	}

	return &eLog
}

func fileModifiedToIncidentLog(e *fsmon.MonitorMessage) *share.CLUSIncidentLog {
	eLog := share.CLUSIncidentLog{
		HostID:      Host.ID,
		HostName:    Host.Name,
		AgentID:     Agent.ID,
		AgentName:   Agent.Name,
		WorkloadID:  e.ID,
		ReportedAt:  time.Now().UTC(),
		FilePath:    e.Path,
		Msg:         e.Msg,
		ProcName:    e.ProcName,
		ProcPath:    e.ProcPath,
		ProcCmds:    e.ProcCmds,
		ProcEffUID:  e.ProcEUid,
		ProcEffUser: e.ProcEUser,
		Count:       e.Count,
		StartAt:     e.StartAt,
		Group:       e.Group,
		Action:      e.Action,
	}

	gInfoLock()
	defer gInfoUnlock()

	if e.ID == "" {
		if e.Package {
			eLog.ID = share.CLUSIncidHostPackageUpdated
			// invalidate scan cache
			gInfo.hostScanCache = nil
		} else if e.Action == share.PolicyActionDeny {
			eLog.ID = share.CLUSIncidHostFileAccessViolation
		} else {
			eLog.ID = share.CLUSIncidHostFileAccessViolation
		}
	} else if c, ok := gInfo.activeContainers[e.ID]; ok {
		eLog.WorkloadName = c.name
		if e.Package {
			eLog.ID = share.CLUSIncidContainerPackageUpdated
			// invalidate scan cache
			c.scanCache = nil
		} else {
			eLog.ID = share.CLUSIncidContainerFileAccessViolation
		}
	} else {
		eLog.WorkloadName = ""
		if e.Package {
			eLog.ID = share.CLUSIncidContainerPackageUpdated
		} else {
			eLog.ID = share.CLUSIncidContainerFileAccessViolation
		}
	}

	return &eLog
}

func processToIncidentLog(s *probe.ProbeProcess, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := &share.CLUSIncidentLog{
		HostID:      Host.ID,
		HostName:    Host.Name,
		AgentID:     Agent.ID,
		AgentName:   Agent.Name,
		WorkloadID:  s.ID,
		ReportedAt:  time.Now().UTC(),
		ProcName:    s.Name,
		ProcPath:    s.Path,
		ProcCmds:    s.Cmds,
		ProcEffUID:  s.EUid,
		ProcEffUser: s.EUser,
		ConnIngress: s.ConnIngress,
		ProcPName:   s.PName,
		ProcPPath:   s.PPath,
		RuleID:      s.RuleID,
		Group:       s.Group,
		Msg:         s.Msg,
		Count:       count,
		StartAt:     start,
	}
	if s.Connection != nil {
		eLog.EtherType = s.Connection.Ether
		eLog.IPProto = s.Connection.Protocol
		eLog.LocalIP = s.Connection.LocIP
		eLog.RemoteIP = s.Connection.RemIP
		eLog.LocalPort = s.Connection.LocPort
		eLog.RemotePort = s.Connection.RemPort
		eLog.LocalPeer = isLocalHostIP(eLog.RemoteIP)
	}

	gInfoRLock()
	defer gInfoRUnlock()

	if s.ID != "" {
		if c, ok := gInfo.activeContainers[s.ID]; ok {
			eLog.WorkloadName = c.name
		}
	}

	return eLog
}

func tunnelToIncidentLog(s *probe.ProbeProcess, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := processToIncidentLog(s, count, start)
	if s.ID == "" {
		eLog.ID = share.CLUSIncidHostTunnel
	} else {
		eLog.ID = share.CLUSIncidContainerTunnel
	}
	eLog.Action = share.PolicyActionViolate
	return eLog
}

func suspicToIncidentLog(s *probe.ProbeProcess, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := processToIncidentLog(s, count, start)
	if s.ID == "" {
		eLog.ID = share.CLUSIncidHostSuspiciousProcess
	} else {
		eLog.ID = share.CLUSIncidContainerSuspiciousProcess
	}
	eLog.Action = share.PolicyActionViolate
	return eLog
}

func procViolationToIncidentLog(s *probe.ProbeProcess, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := processToIncidentLog(s, count, start)
	if s.ID == "" {
		eLog.ID = share.CLUSIncidHostProcessViolation
	} else {
		eLog.ID = share.CLUSIncidContainerProcessViolation
	}
	eLog.Action = share.PolicyActionViolate
	return eLog
}

func procDeniedToIncidentLog(s *probe.ProbeProcess, count int, start time.Time) *share.CLUSIncidentLog {
	eLog := processToIncidentLog(s, count, start)
	if s.ID == "" {
		eLog.ID = share.CLUSIncidHostProcessViolation
	} else {
		eLog.ID = share.CLUSIncidContainerProcessViolation
	}
	eLog.Action = share.PolicyActionDeny
	return eLog
}

func reportIncident(eLog *share.CLUSIncidentLog) {
	log.WithFields(log.Fields{"eLog": *eLog}).Debug("")
	eLog.LogUID = uuid.New().String()
	incidentMutex.Lock()
	incidentLogCache = append(incidentLogCache, eLog)
	incidentMutex.Unlock()
}

func logContainerAudit(name, id string, items []share.CLUSAuditBenchItem, lid share.TLogAudit) {
	alog := &share.CLUSAuditLog{
		ID:           lid,
		HostID:       Host.ID,
		HostName:     Host.Name,
		AgentID:      Agent.ID,
		AgentName:    Agent.Name,
		WorkloadID:   id,
		WorkloadName: name,
		ReportedAt:   time.Now().UTC(),
		Items:        items,
	}

	auditMutex.Lock()
	auditLogCache = append(auditLogCache, alog)
	auditMutex.Unlock()
}

func logHostAudit(items []share.CLUSAuditBenchItem, auditId share.TLogAudit) {
	alog := &share.CLUSAuditLog{
		ID:         auditId,
		HostID:     Host.ID,
		HostName:   Host.Name,
		AgentID:    Agent.ID,
		AgentName:  Agent.Name,
		ReportedAt: time.Now().UTC(),
		Items:      items,
	}

	auditMutex.Lock()
	auditLogCache = append(auditLogCache, alog)
	auditMutex.Unlock()
}
