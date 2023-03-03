package cache

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/controller/access"
	"github.com/zhyocean/neuvector/controller/api"
	"github.com/zhyocean/neuvector/controller/common"
	"github.com/zhyocean/neuvector/controller/rpc"
	"github.com/zhyocean/neuvector/controller/scan"
	"github.com/zhyocean/neuvector/controller/scheduler"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/cluster"
	"github.com/zhyocean/neuvector/share/global"
	scanUtils "github.com/zhyocean/neuvector/share/scan"
	"github.com/zhyocean/neuvector/share/utils"
)

type scannerCache struct {
	scanner  *share.CLUSScanner
	errCount int
}

var scannerCacheMap map[string]*scannerCache = make(map[string]*scannerCache)

// grpc call should timeout in scanReqTimeout
const scanReqTimeout = time.Second * 60
const scanReqSafetyTimeOut = time.Second * 70 // should be longer than scanReqTimeout

const scannerCleanupPeriod = time.Duration(time.Minute * 1)
const scannerClearnupTimeout = time.Second * 20
const scannerClearnupErrorMax = 3

const (
	statusScanNone = iota
	statusScanScheduled
	statusScanning
)

type scanInfo struct {
	initStateLoaded bool
	agentId         string
	status          int
	lastResult      share.ScanErrorCode
	lastScanTime    time.Time
	baseOS          string
	priority        scheduler.Priority
	retry           int
	objType         share.ScanObjectType
	version         string
	cveDBCreateTime string
	brief           *api.RESTScanBrief
	vulTraits       []*common.VulTrait
	filteredTime    time.Time
	idns            []api.RESTIDName
}

type scanTaskInfo struct {
	id   string
	info *scanInfo
}

const maxRetry = 5

var scanCfg share.CLUSScanConfig
var scanScher scheduler.Schd
var scanMap map[string]*scanInfo = make(map[string]*scanInfo)

// Within scanMutex, cacheMutex can be used; but not the other way around.
var scanMutex sync.RWMutex

func scanMutexLock() {
	cctx.MutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Acquire ...")
	scanMutex.Lock()
}

func scanMutexUnlock() {
	scanMutex.Unlock()
	cctx.MutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Released")
}

func scanMutexRLock() {
	cctx.MutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Acquire ...")
	scanMutex.RLock()
}

func scanMutexRUnlock() {
	scanMutex.RUnlock()
	cctx.MutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Released")
}

type scanTask struct {
	id       string
	timerId  string
	priority scheduler.Priority
}

func (t *scanTask) Key() string {
	return t.id
}

func (t *scanTask) Priority() scheduler.Priority {
	return t.priority
}

func (t *scanTask) Print(msg string) {
	cctx.ScanLog.WithFields(log.Fields{"id": t.id}).Debug(msg)
}

func (t *scanTask) rpcScanRunning(scanner string, info *scanInfo) {
	var result *share.ScanResult
	var err error

	if info.objType == share.ScanObjectType_CONTAINER {
		result, err = rpc.ScanRunning(scanner, info.agentId, t.id, share.ScanObjectType_CONTAINER, scanReqTimeout)
	} else if info.objType == share.ScanObjectType_HOST {
		result, err = rpc.ScanRunning(scanner, info.agentId, t.id, share.ScanObjectType_HOST, scanReqTimeout)
		if result != nil {
			// TODO: this is a temp. solution to add RancherOS CVEs. Should be added in the database.
			result, err = appendRancherOSCVE(t.id, result, err)
		}
	} else {
		// Do we need get version again? Can k8s be upgraded without restarting controller?
		cctx.k8sVersion, cctx.ocVersion = global.ORCH.GetVersion()
		result, err = rpc.ScanPlatform(scanner, cctx.k8sVersion, cctx.ocVersion, scanReqTimeout)
	}
	if result == nil || result.Error == share.ScanErrorCode_ScanErrNetwork || err != nil {
		// rpc request not made
		cctx.ScanLog.WithFields(log.Fields{"id": t.id, "error": err}).Error()

		var requeue bool
		scanMutexRLock()
		if info, ok := scanMap[t.id]; ok {
			if (info.priority == scheduler.PriorityHigh || scanCfg.AutoScan == true) && info.retry < maxRetry {
				info.retry++
				cctx.ScanLog.WithFields(log.Fields{
					"id": t.id, "type": info.objType, "retry": info.retry,
				}).Error("Got empty response - requeue")

				info.status = statusScanScheduled
				info.lastResult = share.ScanErrorCode_ScanErrTimeout
				info.lastScanTime = time.Now().UTC()

				requeue = true
			}
		}
		scanMutexRUnlock()

		if requeue {
			updateScanState(t.id, info.objType, api.ScanStatusScheduled)
			scanScher.TaskDone(t, scheduler.TaskActionRequeue)
			return
		}

		result = &share.ScanResult{Error: share.ScanErrorCode_ScanErrTimeout}
	} else if result.Error == share.ScanErrorCode_ScanErrNotSupport ||
		result.Error == share.ScanErrorCode_ScanErrContainerExit {
		result.Error = share.ScanErrorCode_ScanErrNone
	}

	err = putScanReportToCluster(t.id, info, result)
	if err != nil {
		cctx.ScanLog.WithFields(log.Fields{"error": err}).Error("Fail to put report to cluster")
		updateScanState(t.id, info.objType, api.ScanStatusFailed)
	} else {
		updateScanState(t.id, info.objType, api.ScanStatusFinished)
	}

	scanScher.TaskDone(t, scheduler.TaskActionDone)
}

func (t *scanTask) Handler(scanner string) scheduler.Action {
	var ret scheduler.Action
	var ok bool
	var info *scanInfo

	cctx.ScanLog.WithFields(log.Fields{"id": t.id, "scanner": scanner}).Debug()

	scanMutexLock()
	if info, ok = scanMap[t.id]; !ok {
		scanMutexUnlock()
		cctx.ScanLog.WithFields(log.Fields{"id": t.id}).Error("cannot find container")
		ret = scheduler.TaskActionDone
		return ret
	} else {
		// Only wait for result if self is not dispatcher for this task
		/*
			if isDispatcher(info) == false {
				cctx.ScanLog.WithFields(log.Fields{"id": t.id}).Debug("not dispatchable")
				scanMutexUnlock()
				return scheduler.TaskActionRequeueWait
			}
		*/
		info.status = statusScanning
		info.initStateLoaded = false
		ret = scheduler.TaskActionWait
	}
	scanMutexUnlock()

	updateScanState(t.id, info.objType, api.ScanStatusScanning)
	go t.rpcScanRunning(scanner, info)

	return ret
}

func (t *scanTask) StartTimer() {
}

func (t *scanTask) CancelTimer() {
}

func (t *scanTask) Expire() {
}

func enableAutoScan() {
	cctx.ScanLog.Debug("")

	scanCfg.AutoScan = true

	// Queue all workload for scan - this can take a long
	// time if we have lots of containers, so use a separate
	// thread to do it
	if !isScanner() {
		return
	}
	go func() {
		var cnt int
		scanMutexLock()
		all := make([]*scanTaskInfo, 0, len(scanMap))
		for id, info := range scanMap {
			all = append(all, &scanTaskInfo{id, info})
		}
		scanMutexUnlock()
		for _, st := range all {
			if scanCfg.AutoScan == false {
				break
			}
			if st.info.status == statusScanNone || st.info.status == statusScanning {
				task := &scanTask{id: st.id, priority: scheduler.PriorityLow}
				if st.info.status == statusScanning {
					scanScher.DeleteTask(st.id, scheduler.PriorityLow)
				}
				st.info.status = statusScanScheduled
				st.info.priority = scheduler.PriorityLow
				st.info.retry = 0
				scanScher.AddTask(task, false)
				updateScanState(st.id, st.info.objType, api.ScanStatusScheduled)
				cnt++
			} else {
				cctx.ScanLog.WithFields(log.Fields{
					"id": st.id, "status": st.info.status,
				}).Debug("scan status")
			}
		}
		cctx.ScanLog.WithFields(log.Fields{"count": cnt}).Debug("Queued containers")
	}()
}

func disableAutoScan() {
	cctx.ScanLog.WithFields(log.Fields{"isScanner": isScanner()}).Debug("")

	scanCfg.AutoScan = false

	if !isScanner() {
		return
	}
	// cancel all existing workloads queued by auto scan
	go func() {
		scanMutexLock()
		for id, info := range scanMap {
			if info.status == statusScanScheduled && info.priority == scheduler.PriorityLow {
				info.status = statusScanNone
				info.retry = 0
				updateScanState(id, info.objType, api.ScanStatusIdle)
			}
		}
		scanScher.ClearTaskQueue(scheduler.PriorityLow)
		scanMutexUnlock()
	}()
}

func scanObject(id string) {
	cctx.ScanLog.WithFields(log.Fields{"id": id}).Debug("")

	var add, remove bool

	scanMutexLock()
	info, ok := scanMap[id]
	if ok {
		switch info.status {
		case statusScanNone:
			info.status = statusScanScheduled
			info.priority = scheduler.PriorityHigh
			add = true
		case statusScanScheduled, statusScanning:
			remove = true
			add = true
			info.priority = scheduler.PriorityHigh
		}
	} else {
		cctx.ScanLog.WithFields(log.Fields{"id": id}).Error("scan object not found")
	}

	if add && info != nil {
		cctx.ScanLog.WithFields(log.Fields{"id": id, "type": info.objType}).Debug("Add task")

		task := &scanTask{id: id, priority: scheduler.PriorityHigh}
		if remove {
			if scanScher.DeleteTask(id, scheduler.PriorityLow) {
				scanScher.AddTask(task, false)
				updateScanState(id, info.objType, api.ScanStatusScheduled)
			}
		} else {
			scanScher.AddTask(task, false)
			updateScanState(id, info.objType, api.ScanStatusScheduled)
		}
	}
	scanMutexUnlock()
}

func (m CacheMethod) ScanWorkload(id string, acc *access.AccessControl) error {
	if cache := getWorkloadCache(id); cache == nil {
		return common.ErrObjectNotFound
	} else if !acc.Authorize(&share.CLUSWorkloadScanDummy{Domain: cache.workload.Domain}, nil) {
		return common.ErrObjectAccessDenied
	}

	scanObject(id)
	return nil
}

func (m CacheMethod) ScanHost(id string, acc *access.AccessControl) error {
	if cache := getHostCache(id); cache == nil {
		return common.ErrObjectNotFound
	} else if !acc.Authorize(cache.host, nil) {
		return common.ErrObjectAccessDenied
	}

	scanObject(id)
	return nil
}

func (m CacheMethod) ScanPlatform(acc *access.AccessControl) error {
	cctx.ScanLog.Debug()

	if !acc.Authorize(&share.CLUSHost{}, nil) {
		return common.ErrObjectAccessDenied
	}

	scanObject(common.ScanPlatformID)
	return nil
}

// With scan mutex locked
func refreshScanCache(id string, info *scanInfo, vpf common.VPFInterface) {
	vpf.FilterVulTraits(info.vulTraits, info.idns)
	highs, meds := common.CountVulTrait(info.vulTraits)
	brief := fillScanBrief(info, highs, meds)
	info.brief = brief
	info.filteredTime = time.Now()

	switch info.objType {
	case share.ScanObjectType_CONTAINER:
		if c := getWorkloadCache(id); c != nil {
			c.scanBrief = brief
		}
	case share.ScanObjectType_HOST:
		if c := getHostCache(id); c != nil {
			c.scanBrief = brief
		}
	}
}

func scanRefresh(ctx context.Context, vpf common.VPFInterface) {
	log.Debug()

	i := 0

	scanMutexLock()
	ids := make([]string, len(scanMap))
	for id, _ := range scanMap {
		ids[i] = id
		i++
	}
	scanMutexUnlock()

	for _, id := range ids {
		scanMutexLock()
		if info, ok := scanMap[id]; ok {
			// object scanned and vpf has updated
			if info.status == statusScanNone && !info.lastScanTime.IsZero() && vpf.GetUpdatedTime().After(info.filteredTime) {
				refreshScanCache(id, info, vpf)
			}
		}
		scanMutexUnlock()

		select {
		case <-ctx.Done():
			log.Debug("Canceled")
			return
		default:
			// not canceled, continue
		}
	}
}

func scanVulProfUpdate() {
	log.Debug()

	name := share.DefaultVulnerabilityProfileName
	vpf := cacher.GetVulnerabilityProfileInterface(name)

	vpMutex.RLock()
	if c, ok := vpCacheMap[name]; ok {
		ctx, cancel := context.WithCancel(context.Background())
		c.updateCtx, c.updateCancel = ctx, cancel

		go func() {
			log.Debug("Start update cache")
			scanRefresh(ctx, vpf)
			scan.RegistryScanCacheRefresh(ctx, vpf)
			cancel()
			log.Debug("Finish update cache")
		}()
	}
	vpMutex.RUnlock()
}

// This is called on every controller by key update
func scanDone(id string, objType share.ScanObjectType, report *share.CLUSScanReport) {
	cctx.ScanLog.WithFields(log.Fields{
		"id": id, "type": objType, "result": scanUtils.ScanErrorToStr(report.Error),
	}).Debug("")

	var highs, meds []string
	var alives utils.Set // vul names that are not filtered

	scanMutexLock()
	info, ok := scanMap[id]
	if ok {
		info.status = statusScanNone
		info.retry = 0
		info.lastResult = report.Error
		info.lastScanTime = report.ScannedAt
		info.baseOS = report.Namespace
		info.version = report.Version
		info.cveDBCreateTime = report.CVEDBCreateTime

		// Filter and count vulnerabilities
		vpf := cacher.GetVulnerabilityProfileInterface(share.DefaultVulnerabilityProfileName)
		info.vulTraits = common.ExtractVulnerability(report.Vuls)
		alives = vpf.FilterVulTraits(info.vulTraits, info.idns)
		highs, meds = common.GatherVulTrait(info.vulTraits)
		brief := fillScanBrief(info, len(highs), len(meds))
		info.brief = brief
		info.filteredTime = time.Now()

		switch objType {
		case share.ScanObjectType_CONTAINER:
			if c := getWorkloadCache(id); c != nil {
				c.scanBrief = brief
			}
		case share.ScanObjectType_HOST:
			if c := getHostCache(id); c != nil {
				c.scanBrief = brief
			}
		}
	} else {
		cctx.ScanLog.WithFields(log.Fields{"id": id, "type": objType}).Debug("Scan object is gone")
	}
	scanMutexUnlock()

	// all controller should call auditUpdate to record the log, the leader will take action
	if alives != nil {
		clog := scanReport2ScanLog(id, objType, report, highs, meds, "")
		auditUpdate(id, share.EventCVEReport, objType, clog, alives)
	}
}

func (m CacheMethod) GetScannerCount(acc *access.AccessControl) int {
	cacheMutexRLock()
	defer cacheMutexRUnlock()
	if acc.HasGlobalPermissions(share.PERMS_CLUSTER_READ, 0) {
		return len(scannerCacheMap)
	} else {
		var count int
		for _, s := range scannerCacheMap {
			if !acc.Authorize(s.scanner, nil) {
				continue
			}
			count++
		}
		return count
	}
}

func (m CacheMethod) GetAllScanners(acc *access.AccessControl) []*api.RESTScanner {
	cacheMutexRLock()
	defer cacheMutexRUnlock()

	scanners := make([]*api.RESTScanner, 0, len(scannerCacheMap))
	for _, cache := range scannerCacheMap {
		if !acc.Authorize(cache.scanner, nil) {
			continue
		}
		s := cache.scanner
		scanner := api.RESTScanner{
			ID:              s.ID,
			CVEDBVersion:    s.CVEDBVersion,
			CVEDBCreateTime: s.CVEDBCreateTime,
			JoinedTS:        s.JoinedAt.Unix(),
			RPCServer:       s.RPCServer,
			RPCServerPort:   s.RPCServerPort,
		}
		if stats, err := clusHelper.GetScannerStats(s.ID); err != nil {
			log.WithFields(log.Fields{"scanner": s.ID, "error": err}).Error("Failed to get scanner stats")
		} else {
			scanner.Containers = stats.ScannedContainers
			scanner.Hosts = stats.ScannedHosts
			scanner.Images = stats.ScannedImages
			scanner.Serverless = stats.ScannedServerless
		}
		scanners = append(scanners, &scanner)
	}
	return scanners
}

func addScanner(id string) {
	scanScher.AddProcessor(id)
}

func removeScanner(id string) {
	scanScher.DelProcessor(id)
}

func scannerDBChange(newVer string) {
	if isScanner() == false {
		return
	}

	if scanCfg.AutoScan == false {
		return
	}

	go func() {
		scanMutexLock()
		for id, info := range scanMap {
			if info.status == statusScanNone && info.version != newVer {
				info.status = statusScanScheduled
				info.priority = scheduler.PriorityLow
				info.retry = 0
				task := &scanTask{id: id, priority: scheduler.PriorityLow}
				scanScher.AddTask(task, false)
				updateScanState(id, info.objType, api.ScanStatusScheduled)
			}
		}
		scanMutexUnlock()
	}()
}

func scanMapAdd(taskId string, agentId string, idns []api.RESTIDName, objType share.ScanObjectType) {

	scanMutexLock()
	if info, ok := scanMap[taskId]; ok {
		info.agentId = agentId
		scanMutexUnlock()
		return
	}

	info := &scanInfo{
		agentId: agentId,
		status:  statusScanNone,
		objType: objType,
		idns:    idns,
	}
	scanMap[taskId] = info

	// When controller starts, scanStateHandler maybe called before the object is added.
	// We simulate the call if this is a new object
	var skey string
	if objType == share.ScanObjectType_CONTAINER {
		skey = share.CLUSScanStateWorkloadKey(taskId)
	} else if objType == share.ScanObjectType_HOST {
		skey = share.CLUSScanStateHostKey(taskId)
	} else {
		skey = share.CLUSScanStatePlatformKey(taskId)
	}

	// If controller simply restarts or rolling upgraded, don't rescan
	// the object. Only start automatically for new workload.
	if value, err := cluster.Get(skey); err == nil {
		scanMutexUnlock()

		scanStateHandler(cluster.ClusterNotifyAdd, skey, value)
		// avoid scanStateHandler to be processed again if it happens after object is added
		info.initStateLoaded = true
	} else if isScanner() {
		// Always scan the platform even auto-scan is disabled
		if objType == share.ScanObjectType_PLATFORM {
			info.status = statusScanScheduled
			info.priority = scheduler.PriorityHigh
			task := &scanTask{id: taskId, priority: scheduler.PriorityHigh}
			scanScher.AddTask(task, true)
			updateScanState(taskId, info.objType, api.ScanStatusScheduled)
		} else if scanCfg.AutoScan {
			info.status = statusScanScheduled
			info.priority = scheduler.PriorityLow
			task := &scanTask{id: taskId, priority: scheduler.PriorityLow}
			scanScher.AddTask(task, false)
			updateScanState(taskId, info.objType, api.ScanStatusScheduled)
		}
		scanMutexUnlock()
	} else {
		scanMutexUnlock()
	}
}

func scanMapDelete(taskId string) {
	scanMutexLock()
	info, ok := scanMap[taskId]
	if !ok {
		scanMutex.Unlock()
		return
	}
	delete(scanMap, taskId)
	scanMutexUnlock()

	if isScanner() {
		scanScher.DeleteTask(taskId, info.priority)

		/* delete scan report if any */
		var key, skey string
		if info.objType == share.ScanObjectType_CONTAINER {
			key = share.CLUSScanDataWorkloadKey(taskId)
			skey = share.CLUSScanStateWorkloadKey(taskId)
		} else if info.objType == share.ScanObjectType_HOST {
			key = share.CLUSScanDataHostKey(taskId)
			skey = share.CLUSScanStateHostKey(taskId)
		}
		cluster.Delete(key)
		cluster.Delete(skey)
	}
}

func scanWorkloadAdd(id string, param interface{}) {
	// This can be called when the controller restarts, where scanning is not needed if
	// the workload has been scanned.
	cache := param.(*workloadCache)
	workload := cache.workload
	if !common.OEMIgnoreWorkload(workload) {
		idns := []api.RESTIDName{api.RESTIDName{Domains: []string{workload.Domain}}}
		scanMapAdd(id, workload.AgentID, idns, share.ScanObjectType_CONTAINER)
	}
}

func scanWorkloadAgentChange(id string, param interface{}) {
	workload := param.(*workloadCache).workload

	scanMutexLock()
	if info, ok := scanMap[id]; ok {
		info.agentId = workload.AgentID
	}
	scanMutexUnlock()
}

func scanWorkloadDelete(id string, param interface{}) {
	scanMapDelete(id)
}

func scanAgentAdd(id string, param interface{}) {
	// This can be called when the controller restarts, where scanning is not needed if
	// the host has been scanned.
	agent := param.(*agentCache).agent
	scanMapAdd(agent.HostID, id, nil, share.ScanObjectType_HOST)
}

func scanHostDelete(id string, param interface{}) {
	scanMapDelete(id)
}

func scanConfigUpdate(nType cluster.ClusterNotifyType, key string, value []byte) {
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		var cfg share.CLUSScanConfig
		if err := json.Unmarshal(value, &cfg); err != nil {
			cctx.ScanLog.WithFields(log.Fields{"err": err}).Debug("Fail to decode")
			return
		}

		cctx.ScanLog.WithFields(log.Fields{"config": cfg}).Debug("")
		if cfg.AutoScan && !scanCfg.AutoScan {
			enableAutoScan()
		} else if !cfg.AutoScan && scanCfg.AutoScan {
			disableAutoScan()
		}
	case cluster.ClusterNotifyDelete:
		disableAutoScan()
	}
}

func putScanReportToCluster(id string, info *scanInfo, result *share.ScanResult) error {
	cctx.ScanLog.WithFields(log.Fields{
		"id": id, "type": info.objType, "result": scanUtils.ScanErrorToStr(result.Error),
	}).Debug("")

	var key string
	if info.objType == share.ScanObjectType_CONTAINER {
		key = share.CLUSScanDataWorkloadKey(id)
	} else if info.objType == share.ScanObjectType_HOST {
		key = share.CLUSScanDataHostKey(id)
	} else {
		key = share.CLUSScanDataPlatformKey(id)
	}

	now := time.Now().UTC()
	report := share.CLUSScanReport{ScannedAt: now, ScanResult: *result}

	// Write full report and a piece of state data so we only need act upon the state data change notification
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(&report); err == nil {
		zb := utils.GzipBytes(buf.Bytes())
		return cluster.PutBinary(key, zb)
	} else {
		return err
	}
}

func updateScanState(id string, nType share.ScanObjectType, status string) {
	cctx.ScanLog.WithFields(log.Fields{"id": id, "status": status}).Debug("")
	var skey string
	if nType == share.ScanObjectType_CONTAINER {
		skey = share.CLUSScanStateWorkloadKey(id)
	} else if nType == share.ScanObjectType_HOST {
		skey = share.CLUSScanStateHostKey(id)
	} else {
		skey = share.CLUSScanStatePlatformKey(id)
	}

	state := &share.CLUSScanState{Status: status}
	if status == api.ScanStatusFinished {
		state.ScannedAt = time.Now().UTC()
	}
	value, _ := json.Marshal(state)
	cluster.Put(skey, value)
}

func scanStateHandler(nType cluster.ClusterNotifyType, key string, value []byte) {
	cctx.ScanLog.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")

	if nType == cluster.ClusterNotifyDelete {
		return
	}

	var state share.CLUSScanState
	err := json.Unmarshal(value, &state)
	if err != nil {
		return
	}

	id := share.CLUSScanStateKey2ID(key)
	scanMutexRLock()
	info, ok := scanMap[id]
	scanMutexRUnlock()
	if !ok {
		return
	} else if info.initStateLoaded {
		info.initStateLoaded = false
		return
	}

	cctx.ScanLog.WithFields(log.Fields{"key": key, "status": state.Status}).Debug("")

	// For unfinished scan, update status without creating log
	if state.Status == api.ScanStatusScheduled ||
		state.Status == api.ScanStatusIdle ||
		state.Status == api.ScanStatusScanning {
		brief := &api.RESTScanBrief{
			Status: state.Status,
		}
		if info.objType == share.ScanObjectType_CONTAINER {
			if c := getWorkloadCache(id); c != nil {
				c.scanBrief = brief
			}
		} else if info.objType == share.ScanObjectType_HOST {
			if c := getHostCache(id); c != nil {
				c.scanBrief = brief
			}
		}
		if state.Status == api.ScanStatusScheduled {
			info.status = statusScanScheduled
		} else if state.Status == api.ScanStatusIdle {
			info.status = statusScanNone
		} else if state.Status == api.ScanStatusScanning {
			info.status = statusScanning
		}
		return
	}

	// For finished scan, pull the report, call scanDone()
	var objType share.ScanObjectType
	var dkey string
	t := share.CLUSScanStateKey2Type(key)
	if t == "workload" {
		objType = share.ScanObjectType_CONTAINER
		dkey = share.CLUSScanDataWorkloadKey(id)
	} else if t == "host" {
		objType = share.ScanObjectType_HOST
		dkey = share.CLUSScanDataHostKey(id)
	} else {
		objType = share.ScanObjectType_PLATFORM
		dkey = share.CLUSScanDataPlatformKey(id)
	}

	if report := clusHelper.GetScanReport(dkey); report != nil {
		scanDone(id, objType, report)
	}
}

func registryStateHandler(nType cluster.ClusterNotifyType, key string, value []byte) {
	cctx.ScanLog.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")

	name := share.CLUSKeyNthToken(key, 3)

	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		var state share.CLUSRegistryState
		json.Unmarshal(value, &state)
		scan.RegistryStateUpdate(name, &state)
	case cluster.ClusterNotifyDelete:
		// State is deleted when registry deleted. No handling here.
	}
}

func registryImageStateHandler(nType cluster.ClusterNotifyType, key string, value []byte) {
	cctx.ScanLog.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")

	name := share.CLUSKeyNthToken(key, 3)
	id := share.CLUSKeyNthToken(key, 4)

	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		var sum share.CLUSRegistryImageSummary
		json.Unmarshal(value, &sum)

		vpf := cacher.GetVulnerabilityProfileInterface(share.DefaultVulnerabilityProfileName)
		alives, highs, meds := scan.RegistryImageStateUpdate(name, id, &sum, vpf)

		key := share.CLUSRegistryImageDataKey(name, id)
		if report := clusHelper.GetScanReport(key); report != nil {
			if alives != nil {
				clog := scanReport2ScanLog(id, share.ScanObjectType_IMAGE, report, highs, meds, name)
				auditUpdate(id, share.EventCVEReport, share.ScanObjectType_IMAGE, clog, alives)
			}

			clog := scanReport2BenchLog(id, share.ScanObjectType_IMAGE, report, name)
			benchUpdate(share.EventCompliance, clog)
		}

	case cluster.ClusterNotifyDelete:
		scan.RegistryImageStateUpdate(name, id, nil, nil)
	}
}

func ScannerUpdateHandler(nType cluster.ClusterNotifyType, key string, value []byte, modifyIdx uint64) {
	log.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug("")
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		// For the built-in scanner, the CVEDB version can change
		var s share.CLUSScanner
		if err := json.Unmarshal(value, &s); err == nil {
			log.WithFields(log.Fields{"scanner": s}).Info("Add or update scanner")

			if s.ID == share.CLUSScannerDBVersionID {
				// Dummy scanner to indicate db version change. It should not stored in the map.
				newStore := fmt.Sprintf("%s%s/", share.CLUSScannerDBStore, s.CVEDBVersion)

				newDB := &share.CLUSScannerDB{
					CVEDBVersion:    s.CVEDBVersion,
					CVEDBCreateTime: s.CVEDBCreateTime,
					CVEDB:           make(map[string]*share.ScanVulnerability),
				}

				// Reassemble
				dbs := clusHelper.GetScannerDB(newStore)
				for _, db := range dbs {
					for _, cve := range db.CVEDB {
						newDB.CVEDB[cve.Name] = cve
					}
				}

				log.WithFields(log.Fields{"cvedb": newDB.CVEDBVersion, "entries": len(newDB.CVEDB)}).Info()

				common.SetScannerDB(newDB)
				scan.ScannerDBChange(newDB)
				scannerDBChange(newDB.CVEDBVersion)
			} else {
				// Real Scanner
				cacheMutexLock()
				if exist, ok := scannerCacheMap[s.ID]; ok {
					exist.scanner = &s
					exist.errCount = 0
				} else {
					scannerCacheMap[s.ID] = &scannerCache{scanner: &s, errCount: 0}
				}
				cacheMutexUnlock()

				if !s.BuiltIn {
					rpc.AddScanner(&s)
					scan.AddScanner(s.ID)
					addScanner(s.ID)
				} else if s.ID == localDev.Ctrler.ID {
					rpc.AddScanner(&s)
					scan.AddScanner(s.ID)
					addScanner(s.ID)
				}
			}
		}
	case cluster.ClusterNotifyDelete:
		id := share.CLUSScannerKey2ID(key)
		if id == share.CLUSScannerDBVersionID {
			log.WithFields(log.Fields{"scanner": id}).Error("Cannot delete dummy db version scanner")
		} else {
			log.WithFields(log.Fields{"scanner": id}).Info("Delete scaner")

			cacheMutexLock()
			delete(scannerCacheMap, id)
			cacheMutexUnlock()

			rpc.RemoveScanner(id)
			scan.RemoveScanner(id)
			removeScanner(id)
		}
	}
}

func ScanUpdateHandler(nType cluster.ClusterNotifyType, key string, value []byte, modifyIdx uint64) {
	object := share.CLUSScanKey2Subject(key)
	switch object {
	case "report":
		scanStateHandler(nType, key, value)
	case "registry":
		registryStateHandler(nType, key, value)
	case "image":
		registryImageStateHandler(nType, key, value)
	}
}

func scanLicenseUpdate(id string, param interface{}) {

	// Cache lock must be within scan lock, so get the map first
	wls := make(map[string]struct{ a, d string }, len(wlCacheMap))
	hosts := make(map[string]string, len(agentCacheMap))
	cacheMutexRLock()
	for id, cache := range wlCacheMap {
		wls[id] = struct{ a, d string }{a: cache.workload.AgentID, d: cache.workload.Domain}
	}
	for id, cache := range agentCacheMap {
		hosts[cache.agent.HostID] = id
	}
	cacheMutexRUnlock()

	for id, m := range wls {
		idns := []api.RESTIDName{api.RESTIDName{Domains: []string{m.d}}}
		scanMapAdd(id, m.a, idns, share.ScanObjectType_CONTAINER)
	}
	for id, a := range hosts {
		scanMapAdd(id, a, nil, share.ScanObjectType_HOST)
	}
	scanMapAdd(common.ScanPlatformID, "", nil, share.ScanObjectType_PLATFORM)
}

func scanBecomeScanner() {
	log.Debug()

	scanMutexLock()
	for taskId, info := range scanMap {
		if info.status != statusScanNone {
			info.status = statusScanScheduled
			if info.priority == scheduler.PriorityHigh {
				task := &scanTask{id: taskId, priority: scheduler.PriorityHigh}
				scanScher.AddTask(task, true)
			} else {
				task := &scanTask{id: taskId, priority: scheduler.PriorityLow}
				scanScher.AddTask(task, false)
			}
			updateScanState(taskId, info.objType, api.ScanStatusScheduled)
		}
	}
	scanMutexUnlock()
}

func scanInit() {
	scanScher.Init()

	acc := access.NewReaderAccessControl()
	cfg, _ := clusHelper.GetScanConfigRev(acc)
	scanCfg = *cfg
}

/*----------------------------------------------------------------------*/
/*----------------------------------------------------------------------*/
func (m CacheMethod) GetScanConfig(acc *access.AccessControl) (*api.RESTScanConfig, error) {
	cctx.ScanLog.Debug("")

	if !acc.Authorize(&scanCfg, nil) {
		return nil, common.ErrObjectAccessDenied
	}

	var cfg *api.RESTScanConfig
	if scanCfg.AutoScan == true {
		cfg = &api.RESTScanConfig{AutoScan: true}
	} else {
		cfg = &api.RESTScanConfig{AutoScan: false}
	}

	return cfg, nil
}

func (m CacheMethod) GetScanStatus(acc *access.AccessControl) (*api.RESTScanStatus, error) {
	var status api.RESTScanStatus

	if !acc.Authorize(&status, nil) {
		return nil, common.ErrObjectAccessDenied
	}

	scanMutexRLock()
	defer scanMutexRUnlock()

	for _, info := range scanMap {
		if info.status == statusScanScheduled {
			status.Scheduled++
		} else if info.status == statusScanning {
			status.Scanning++
		} else if !info.lastScanTime.IsZero() {
			status.Scanned++
		}
	}
	sdb := common.GetScannerDB()
	status.CVEDBVersion = sdb.CVEDBVersion
	status.CVEDBCreateTime = sdb.CVEDBCreateTime
	return &status, nil
}

func fillScanBrief(info *scanInfo, high, med int) *api.RESTScanBrief {
	brief := &api.RESTScanBrief{
		CVEDBVersion:    info.version,
		CVEDBCreateTime: info.cveDBCreateTime,
	}

	switch info.status {
	case statusScanScheduled:
		brief.Status = api.ScanStatusScheduled
	case statusScanning:
		brief.Status = api.ScanStatusScanning
	case statusScanNone:
		if !info.lastScanTime.IsZero() {
			if info.lastResult == share.ScanErrorCode_ScanErrNone {
				brief.Status = api.ScanStatusFinished
				brief.HighVuls = high
				brief.MedVuls = med
			} else if info.lastResult == share.ScanErrorCode_ScanErrNotSupport ||
				info.lastResult == share.ScanErrorCode_ScanErrContainerExit {
				brief.Status = api.ScanStatusFinished
			} else {
				brief.Status = api.ScanStatusFailed
			}
			brief.ScannedTimeStamp = info.lastScanTime.Unix()
			brief.ScannedAt = api.RESTTimeString(info.lastScanTime)
			brief.Result = scanUtils.ScanErrorToStr(info.lastResult)
		} else {
			brief.Status = api.ScanStatusIdle
		}
		brief.BaseOS = info.baseOS
	}

	return brief
}

func scanBrief2REST(info *scanInfo) *api.RESTScanBrief {
	var r api.RESTScanBrief

	// What is stored in info.brief is the last scan result. If an entity is in scanning state,
	// set its status explicitly. NOTE: scanBrief in the workload is always the last scan result.
	switch info.status {
	case statusScanScheduled:
		r.Status = api.ScanStatusScheduled
	case statusScanning:
		r.Status = api.ScanStatusScanning
	case statusScanNone:
		if !info.lastScanTime.IsZero() {
			if info.brief != nil {
				r = *info.brief
			} else {
				r.ScannedTimeStamp = info.lastScanTime.Unix()
				r.ScannedAt = api.RESTTimeString(info.lastScanTime)
				r.Result = scanUtils.ScanErrorToStr(info.lastResult)
			}
		} else {
			r.Status = api.ScanStatusIdle
		}
		r.BaseOS = info.baseOS
	}
	sdb := common.GetScannerDB()
	r.CVEDBVersion = sdb.CVEDBVersion
	r.CVEDBCreateTime = sdb.CVEDBCreateTime
	return &r
}

func (m CacheMethod) GetVulnerabilityReport(id, showTag string) ([]*api.RESTVulnerability, error) {
	scanMutexRLock()
	defer scanMutexRUnlock()

	if info, ok := scanMap[id]; ok {
		vpf := cacher.GetVulnerabilityProfileInterface(share.DefaultVulnerabilityProfileName)
		if info.status == statusScanNone && !info.lastScanTime.IsZero() && vpf.GetUpdatedTime().After(info.filteredTime) {
			refreshScanCache(id, info, vpf)
		}

		sdb := common.GetScannerDB()
		vuls := common.FillVulDetails(sdb.CVEDB, info.baseOS, info.vulTraits, showTag)
		return vuls, nil
	} else {
		return nil, common.ErrObjectNotFound
	}
}

func (m CacheMethod) GetScanPlatformSummary(acc *access.AccessControl) (*api.RESTScanPlatformSummary, error) {
	scanMutexRLock()
	defer scanMutexRUnlock()

	if acc.Authorize(&share.CLUSHost{}, nil) {
		if info, ok := scanMap[common.ScanPlatformID]; ok {
			brief := scanBrief2REST(info)
			s := &api.RESTScanPlatformSummary{RESTScanBrief: *brief}
			s.Platform, s.K8sVersion, s.OCVersion = m.GetPlatform()
			return s, nil
		} else {
			return nil, common.ErrObjectNotFound
		}
	} else {
		return nil, common.ErrObjectAccessDenied
	}
}
