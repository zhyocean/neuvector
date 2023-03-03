package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/controller/access"
	"github.com/zhyocean/neuvector/controller/api"
	"github.com/zhyocean/neuvector/controller/common"
	"github.com/zhyocean/neuvector/controller/scheduler"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/cluster"
	"github.com/zhyocean/neuvector/share/httptrace"
	scanUtils "github.com/zhyocean/neuvector/share/scan"
	"github.com/zhyocean/neuvector/share/utils"
)

const (
	registryErrMsgConnect = "Failed to connect to the registry"
	registryErrMsgImage   = "Failed to get scanning image list"
	registryErrMsgAuth    = "Authentication error"

	scanReqTimeout       = time.Minute * 20
	scanReqSafetyTimeOut = time.Minute * 30 // Should be longer than scanReqTimeout

	scanPersistImageExtra = 32

	registryRepoScanName = "_repo_scan"
)

type scanContext struct {
	ctx        context.Context
	cancel     context.CancelFunc
	scheduling bool
}

type pollContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// This structure is derived from image summary and scan report. Mostly used for admisssion control
type imageInfoCache struct {
	highVuls        int
	medVuls         int
	highVulsWithFix int
	vulScore        float32
	vulTraits       []*common.VulTrait
	vulInfo         map[string]map[string]share.CLUSScannedVulInfo // 1st key is "high"/"medium". 2nd key is "{vul_name}::{package_name}"
	lowVulInfo      []share.CLUSScannedVulInfoSimple
	layers          []string
	envs            []string
	labels          map[string]string
	secrets         []share.ScanSecretLog
	setIDPerm       []share.ScanSetIdPermLog
	filteredTime    time.Time
}

type Registry struct {
	public    bool
	config    *share.CLUSRegistryConfig
	state     *share.CLUSRegistryState
	summary   map[string]*share.CLUSRegistryImageSummary
	cache     map[string]*imageInfoCache
	image2ID  map[share.CLUSImage]string
	digest2ID map[string]string
	taskQueue utils.Set
	sctx      *scanContext
	pctx      *pollContext
	// Keep driver in registry so it doesn't login for every scan
	driver    registryDriver // TODO: add the idle logout logic
	backupDrv registryDriver
	errDetail string
	stateMux  sync.Mutex
}

var repoScanRegistry *Registry
var regMap map[string]*Registry = make(map[string]*Registry)
var regMux sync.RWMutex

// aquire regLock first, and then stateLock
func regLock() {
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Acquire ...")
	regMux.Lock()
}

func regUnlock() {
	regMux.Unlock()
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Released ...")
}

func regReadLock() {
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Acquire ...")
	regMux.RLock()
}

func regReadUnlock() {
	regMux.RUnlock()
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Released ...")
}

func (rs *Registry) stateLock() {
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Acquire ...")
	rs.stateMux.Lock()
}

func (rs *Registry) stateUnlock() {
	rs.stateMux.Unlock()
	smd.mutexLog.WithFields(log.Fields{"goroutine": utils.GetGID()}).Debug("Released ...")
}

func regMapToArray() []*Registry {
	regReadLock()
	defer regReadUnlock()
	rss := make([]*Registry, len(regMap))

	i := 0
	for _, rs := range regMap {
		rss[i] = rs
		i++
	}
	return rss
}

func regMapLookup(name string) (*Registry, bool) {
	regReadLock()
	defer regReadUnlock()

	rs, ok := regMap[name]
	return rs, ok
}

var regScher *scheduler.Schd

func registryInit() {
	if regScher == nil {
		regScher = &scheduler.Schd{}
		regScher.Init()
	}

	// Get all registry config first. We will get notified again.
	newRepoScanRegistry()
	configs := clusHelper.GetAllRegistry()
	for _, config := range configs {
		regLock()
		regMap[config.Name] = newRegistry(config)
		regUnlock()
	}
}

func becomeScanner() {
	log.Debug()

	regs := regMapToArray()

	for _, reg := range regs {
		reg.stateLock()
		if reg.state.Status == api.RegistryStatusScanning {
			reg.resumeScan()
		} else if reg.config.Schedule == api.ScanSchAuto {
			reg.resumeScan()
		}
		if reg.config.Schedule == api.ScanSchPeriodical {
			ctx, cancel := context.WithCancel(context.Background())
			reg.pctx = &pollContext{ctx: ctx, cancel: cancel}
			go reg.polling(ctx)
		}
		reg.stateUnlock()
	}
}

func RegistryConfigHandler(nType cluster.ClusterNotifyType, key string, value []byte) {
	log.WithFields(log.Fields{"type": cluster.ClusterNotifyName[nType], "key": key}).Debug()

	name := share.CLUSKeyNthToken(key, 3)
	switch nType {
	case cluster.ClusterNotifyAdd, cluster.ClusterNotifyModify:
		var config share.CLUSRegistryConfig
		json.Unmarshal(value, &config)

		var oldFilters []string
		oldSchedule := api.ScanSchManual
		var credChanged bool

		reg, ok := regMapLookup(config.Name)

		if ok && reg.config.Name != "" {
			oldSchedule = reg.config.Schedule
			oldFilters = reg.config.Filters

			oldCfg := reg.config
			reg.config = &config

			// Clear last error if configuration is changed.
			reg.state.ErrMsg = ""
			public := isPublicRegistry(&config)
			reg.driver.SetConfig(&config)
			reg.backupDrv.SetConfig(&config)

			if oldCfg.Registry != config.Registry || oldCfg.AuthWithToken != config.AuthWithToken ||
				(oldCfg.AuthWithToken && oldCfg.AuthToken != config.AuthToken) ||
				(!oldCfg.AuthWithToken && (oldCfg.Username != config.Username || oldCfg.Password != config.Password)) ||
				public != reg.public {
				// URL or credential changed, stop scan and force logout
				credChanged = true
				reg.driver.Logout(true)
				reg.backupDrv.Logout(true)
				// if the public change, need to renew the driver type.
				if public != reg.public {
					reg.public = public
					reg.driver = newRegistryDriver(reg.config, reg.public, new(httptrace.NopTracer))
				}
				if isScanner() {
					reg.stateLock()
					if reg.state.Status == api.RegistryStatusScanning {
						// stopScan() will be called
						state := share.CLUSRegistryState{Status: api.RegistryStatusIdle, StartedAt: reg.state.StartedAt}
						clusHelper.PutRegistryState(reg.config.Name, &state)
					}
					reg.stateUnlock()
				}
			}
			if isScanner() {
				if oldCfg.Schedule == api.ScanSchPeriodical &&
					config.Schedule != api.ScanSchPeriodical && reg.pctx != nil {
					reg.pctx.cancel()
					reg.pctx = nil
				} else if oldCfg.Schedule != api.ScanSchPeriodical &&
					config.Schedule == api.ScanSchPeriodical {
					ctx, cancel := context.WithCancel(context.Background())
					reg.pctx = &pollContext{ctx: ctx, cancel: cancel}
					go reg.polling(ctx)
				} else if config.Schedule == api.ScanSchPeriodical &&
					oldCfg.PollPeriod != config.PollPeriod && reg.pctx != nil {
					reg.pctx.cancel()
					ctx, cancel := context.WithCancel(context.Background())
					reg.pctx = &pollContext{ctx: ctx, cancel: cancel}
					go reg.polling(ctx)
				}
			}

			// Assume that state is always created way after config is created, so no check
			// of state here.
		} else {
			oldFilters = make([]string, 0)

			reg = newRegistry(&config)
			// put recovery images summary into new created registry
			if oldReg, ok := regMapLookup(config.Name); ok && len(oldReg.summary) > 0 {
				reg.summary = oldReg.summary
				reg.image2ID = oldReg.image2ID
				reg.digest2ID = oldReg.digest2ID
			}
			if isScanner() && reg.config.Schedule == api.ScanSchPeriodical {
				ctx, cancel := context.WithCancel(context.Background())
				reg.pctx = &pollContext{ctx: ctx, cancel: cancel}
				go reg.polling(ctx)
			}

			regLock()
			regMap[config.Name] = reg
			regUnlock()
		}

		if reg.config.Type == share.RegistryTypeOpenShift {
			if oldSchedule == api.ScanSchManual && config.Schedule == api.ScanSchAuto {
				registerImageBank(api.RegistryImageSourceOpenShift, reg.config.Name)
			} else if oldSchedule == api.ScanSchAuto && config.Schedule == api.ScanSchManual {
				deregisterImageBank(api.RegistryImageSourceOpenShift, reg.config.Name)
			}
		}

		if credChanged {
			return
		}

		if isScanner() {
			if oldSchedule == api.ScanSchManual && config.Schedule == api.ScanSchAuto {
				reg.stateLock()
				if reg.state.Status == api.RegistryStatusIdle {
					// Start scanning if not
					state := share.CLUSRegistryState{Status: api.RegistryStatusScanning, StartedAt: time.Now().Unix()}
					clusHelper.PutRegistryState(reg.config.Name, &state)
				}
				reg.stateUnlock()
			} else if config.Schedule == api.ScanSchAuto && !reflect.DeepEqual(oldFilters, config.Filters) {
				// Filter changes (including order change) with auto-scan enabled
				/*
					if reg.state.Status == api.RegistryStatusIdle {
						// Start scanning if not
						state := share.CLUSRegistryState{Status: api.RegistryStatusScanning, StartedAt: time.Now().Unix()}
						clusHelper.PutRegistryState(reg.config.Name, &state)
					} else if reg.sctx != nil && !reg.sctx.refreshing {
						// We don't want to block config change handler for too long, but if filters keeps changing, we don't want
						// them to run in parallel. Use a flag to discard later changes. We could use another context to cancel
						// the current and keep last one.
						reg.sctx.refreshing = true
						go reg.imageScanRefresh(false)
					}
				*/
			}
		}

	case cluster.ClusterNotifyDelete:
		regLock()
		reg, ok := regMap[name]
		if ok {
			// It's possible that new scan get kicked in, we just cancel it here.
			if reg.sctx != nil {
				reg.sctx.cancel()
				reg.sctx = nil
				reg.driver.Logout(true)
			}
			if reg.pctx != nil {
				reg.pctx.cancel()
				reg.pctx = nil
			}
			// It's possible backupDrv never logged in
			reg.backupDrv.Logout(true)
			delete(regMap, name)
		}
		regUnlock()

		if ok && isScanner() {
			reg.cleanup()

			if reg.config.Schedule == api.ScanSchAuto {
				if reg.config.Type == share.RegistryTypeOpenShift {
					deregisterImageBank(api.RegistryImageSourceOpenShift, reg.config.Name)
				}
			}
		}
	}
}

// Allow manual start/stop scan even if auto-scan is enabled, scan can be restarted manually or when
// new images added
func RegistryStateUpdate(name string, state *share.CLUSRegistryState) {
	// Assume that state is always created way after config is created, so ignore if config doesn't exist
	reg, ok := regMapLookup(name)

	if ok {
		smd.scanLog.WithFields(log.Fields{"registry": name, "old-state": reg.state, "state": state}).Debug("")

		reg.stateLock()
		defer reg.stateUnlock()
		oldStatus := reg.state.Status
		reg.state = state

		if isScanner() {
			if oldStatus == api.RegistryStatusIdle && state.Status == api.RegistryStatusScanning {
				// If sctx is not nil, scan is running, this happens when scan is triggered by new images.
				// Must create context here so we can cancel it.
				if reg.sctx == nil {
					reg.startScan()
				} else {
					smd.scanLog.WithFields(log.Fields{"registry": name}).Debug("skip start - scanning")
				}
			} else if oldStatus == api.RegistryStatusScanning && state.Status == api.RegistryStatusIdle {
				// sctx could be nil if scan failed to start
				if reg.sctx != nil {
					reg.stopScan()
				} else {
					smd.scanLog.WithFields(log.Fields{"registry": name}).Debug("skip stop - not scanning")
				}
			}
		}
	}
}

func RegistryImageStateUpdate(name, id string, sum *share.CLUSRegistryImageSummary, vpf common.VPFInterface) (utils.Set, []string, []string) {
	smd.scanLog.WithFields(log.Fields{"registry": name, "id": id}).Debug()

	var rs *Registry

	// We assume that report is always created way after config is created, so ignore if config doesn't exist
	if name == registryRepoScanName {
		rs = repoScanRegistry
	} else if rs, _ = regMapLookup(name); rs == nil {
		return nil, nil, nil
	}

	var c *imageInfoCache
	var highs, meds []string
	var alives utils.Set // vul names that are not filtered

	if sum != nil && sum.Status == api.ScanStatusFinished {
		key := share.CLUSRegistryImageDataKey(name, id)
		if report := clusHelper.GetScanReport(key); report != nil {
			c = &imageInfoCache{}

			// Filter the vulnerabilities
			c.filteredTime = time.Now()
			c.vulTraits = common.ExtractVulnerability(report.Vuls)
			if vpf != nil {
				alives = vpf.FilterVulTraits(c.vulTraits, images2IDNames(rs, sum))
			} else {
				alives = utils.NewSet()
				for _, t := range c.vulTraits {
					alives.Add(t.Name)
				}
			}

			highs, meds, c.highVulsWithFix, c.vulScore, c.vulInfo, c.lowVulInfo = countVuln(report.Vuls, alives)
			c.highVuls = len(highs)
			c.medVuls = len(meds)
			c.envs = report.Envs
			c.labels = report.Labels

			c.layers = make([]string, len(report.Layers))
			for i, l := range report.Layers {
				c.layers[i] = l.Digest
			}
			if report.Secrets != nil {
				c.secrets = make([]share.ScanSecretLog, len(report.Secrets.Logs))
				for i, s := range report.Secrets.Logs {
					c.secrets[i] = *s
				}
			}
			c.setIDPerm = make([]share.ScanSetIdPermLog, len(report.SetIdPerms))
			for i, s := range report.SetIdPerms {
				c.setIDPerm[i] = *s
			}
		}
	}

	rs.stateLock()
	defer rs.stateUnlock()

	if sum != nil {
		for _, image := range sum.Images {
			rs.image2ID[image] = id
		}
		rs.digest2ID[sum.Digest] = id
		rs.summary[id] = sum

		if c != nil {
			rs.cache[id] = c
		}
	} else if sum, ok := rs.summary[id]; ok {
		for _, image := range sum.Images {
			delete(rs.image2ID, image)
		}
		delete(rs.digest2ID, sum.Digest)
		delete(rs.summary, id)
		delete(rs.cache, id)
	}

	return alives, highs, meds
}

func RegistryScanCacheRefresh(ctx context.Context, vpf common.VPFInterface) {
	log.Debug()

	regs := regMapToArray()
	for _, rs := range regs {
		rs.stateLock()
		for id, sum := range rs.summary {
			if sum.Status == api.ScanStatusFinished {
				if c, ok := rs.cache[id]; ok {
					refreshScanCache(rs, id, sum, c, vpf)
				}
			}
			select {
			case <-ctx.Done():
				rs.stateUnlock()
				smd.scanLog.Debug("Canceled")
				return
			default:
				// not canceled, continue
			}
		}
		rs.stateUnlock()
	}
}

func isPublicRegistry(cfg *share.CLUSRegistryConfig) bool {
	return strings.Contains(cfg.Registry, ".docker.com") || strings.Contains(cfg.Registry, ".docker.io") ||
		cfg.Type == share.RegistryTypeRedhat
}

func newRegistryDriver(cfg *share.CLUSRegistryConfig, public bool, tracer httptrace.HTTPTrace) registryDriver {
	baseDriver := base{
		regURL:      cfg.Registry,
		proxy:       GetProxy(cfg.Registry),
		scanLayers:  cfg.ScanLayers,
		scanSecrets: !cfg.DisableFiles,
		tracer:      tracer,
	}

	if cfg.Type == share.RegistryTypeJFrog {
		return &jfrog{base: baseDriver, mode: cfg.JfrogMode, aql: cfg.JfrogAQL}
	} else if cfg.Type == share.RegistryTypeOpenShift {
		return &openshift{base: baseDriver}
	} else if cfg.Type == share.RegistryTypeAWSECR {
		return &awsDriver{base: baseDriver}
	} else if cfg.Type == share.RegistryTypeDocker && public {
		return &dockerhub{base: baseDriver}
	} else if cfg.Type == share.RegistryTypeGCR {
		return &gcrDriver{base: baseDriver}
	} else if cfg.Type == share.RegistryTypeGitlab {
		return &gitlab{base: baseDriver}
	} else if cfg.Type == share.RegistryTypeIBMCloud {
		return &ibmcloud{base: baseDriver}
	} else {
		return &baseDriver
	}
}

func newRegistry(config *share.CLUSRegistryConfig) *Registry {
	smd.scanLog.WithFields(log.Fields{"registry": config.Name}).Debug("")

	rs := &Registry{
		config:    config,
		state:     &share.CLUSRegistryState{Status: api.RegistryStatusIdle},
		summary:   make(map[string]*share.CLUSRegistryImageSummary),
		cache:     make(map[string]*imageInfoCache),
		image2ID:  make(map[share.CLUSImage]string),
		digest2ID: make(map[string]string),
		taskQueue: utils.NewSet(),
		public:    isPublicRegistry(config),
	}

	rs.driver = newRegistryDriver(rs.config, rs.public, new(httptrace.NopTracer))
	rs.backupDrv = newRegistryDriver(rs.config, rs.public, new(httptrace.NopTracer))
	return rs
}

func newRepoScanRegistry() {
	repoScanRegistry = &Registry{
		config:    &share.CLUSRegistryConfig{Name: registryRepoScanName, Type: share.RegistryTypeDocker},
		state:     &share.CLUSRegistryState{Status: api.RegistryStatusIdle},
		summary:   make(map[string]*share.CLUSRegistryImageSummary),
		cache:     make(map[string]*imageInfoCache),
		image2ID:  make(map[share.CLUSImage]string),
		digest2ID: make(map[string]string),
		taskQueue: utils.NewSet(),
	}
}

func (rs *Registry) newScanContext() (*scanContext, error) {
	if err, msg := rs.driver.Login(rs.config); err != nil {
		rs.errDetail = msg
		return nil, err
	} else {
		ctx, cancel := context.WithCancel(context.Background())
		return &scanContext{ctx: ctx, cancel: cancel}, nil
	}
}

// no lock
func (rs *Registry) getScanImages(sctx *scanContext, drv registryDriver, dryrun bool) (map[string]utils.Set, int, error) {
	sctx.scheduling = true
	defer func() { sctx.scheduling = false }()

	rs.driver.GetTracer().SetPhase("Get all images")

	// not all driver support this
	allImages, err := drv.GetAllImages()

	if sctx.ctx != nil {
		select {
		case <-sctx.ctx.Done():
			smd.scanLog.Debug("Registry scan canceled")
			return nil, 0, err
		default:
			// not canceled, continue
		}
	}

	rs.driver.GetTracer().SetPhase("Get registry repository list")

	// Get a list of repository. Tag is not expanded yet.
	// Different filter might give the same repo list, but their tag filter could be different,
	// so repos cannot be merged until tags are expanded.
	err = nil
	imageTagFilters := make([]*share.CLUSImage, 0)
	for _, filter := range rs.config.ParsedFilters {
		smd.scanLog.WithFields(log.Fields{"filter": filter}).Debug("")

		var limit int
		if rs.public {
			limit = rs.config.RepoLimit
		}

		var repos []*share.CLUSImage
		if allImages != nil {
			prefix := fmt.Sprintf("%s/", filter.Org)
			matchAll := (filter.Org == "" && filter.Repo == ".*")
			for r, _ := range allImages {
				if matchAll || (filter.Org != "" && strings.HasPrefix(r.Repo, prefix)) {
					// create a new CLUSImage because &r points one same location
					repos = append(repos, &share.CLUSImage{Repo: r.Repo, RegMod: r.RegMod})
				}
			}
		} else {
			repos, err = drv.GetRepoList(filter.Org, filter.Repo, limit)
		}
		if err != nil {
			smd.scanLog.WithFields(log.Fields{"error": err}).Error("Failed to get repository list")
			continue
		}

		// for docker hub single repo
		if rs.config.Type == share.RegistryTypeDocker && rs.public &&
			filter.Org == "" && !strings.Contains(filter.Repo, "*") {
			filter.Org = "library"
		}
		var filteredRepos []*share.CLUSImage
		// Only filter image 'org' by registry creater domains for Openshift registry
		if rs.config.Type == share.RegistryTypeOpenShift {
			filteredRepos, err = filterRepos(repos, filter, rs.config.CreaterDomains, limit)
		} else {
			filteredRepos, err = filterRepos(repos, filter, nil, limit)
		}
		if err != nil {
			smd.scanLog.WithFields(log.Fields{"error": err}).Error("Failed to filter repository list")
			continue
		}

		for _, repo := range filteredRepos {
			repo.Tag = filter.Tag
			imageTagFilters = append(imageTagFilters, repo)
		}

		if sctx.ctx != nil {
			select {
			case <-sctx.ctx.Done():
				smd.scanLog.Debug("Registry scan canceled")
				return nil, 0, err
			default:
				// not canceled, continue
			}
		}
	}

	smd.scanLog.WithFields(log.Fields{"count": len(imageTagFilters)}).Debug("image with tag filter")

	if len(imageTagFilters) == 0 && err != nil {
		return nil, 0, err
	}

	rs.driver.GetTracer().SetPhase("Get registry repository tag list")

	// expand tags
	itfList := make([]*share.CLUSImage, 0)
	tagList := make([][]string, 0)
	for _, itf := range imageTagFilters {
		var tags []string
		if allImages != nil {
			var ok bool
			lookup := share.CLUSImage{Domain: itf.Domain, Repo: itf.Repo, RegMod: itf.RegMod}
			if tags, ok = allImages[lookup]; !ok {
				err = common.ErrObjectNotFound
			}
		} else {
			tags, err = drv.GetTagList(itf.Domain, itf.Repo, itf.Tag)
		}
		if err != nil {
			smd.scanLog.WithFields(log.Fields{"error": err}).Error("Failed to get repository tag list")
			continue
		}

		var limit int
		if rs.public {
			limit = rs.config.TagLimit
		}
		var filteredTags []string
		filteredTags, err = filterTags(tags, itf.Tag, limit)
		if err != nil {
			smd.scanLog.WithFields(log.Fields{"error": err}).Error("Failed to filter repository tag list")
			continue
		}

		itfList = append(itfList, itf)
		tagList = append(tagList, filteredTags)
		smd.scanLog.WithFields(log.Fields{"image": itf, "tags": len(filteredTags)}).Debug()

		if sctx.ctx != nil {
			select {
			case <-sctx.ctx.Done():
				smd.scanLog.Debug("Registry scan canceled")
				return nil, 0, err
			default:
				// not canceled, continue
			}
		}
	}

	if !dryrun {
		imageMap, total := rs.scheduleScanImages(sctx, drv, itfList, tagList)
		smd.scanLog.WithFields(log.Fields{"count": total}).Debug("total images")
		return imageMap, total, nil
	} else {
		rs.driver.GetTracer().SetPhase("Get registry image manifest")

		var total int
		imageMap := make(map[string]utils.Set)

		for i := 0; i < len(itfList); i++ {
			itf := itfList[i]
			tags := tagList[i]

			for _, tag := range tags {
				info, errCode := drv.GetImageMeta(sctx.ctx, itf.Domain, itf.Repo, tag)
				if errCode != share.ScanErrorCode_ScanErrNone {
					smd.scanLog.WithFields(log.Fields{
						"repo": itf, "tag": tag, "error": scanUtils.ScanErrorToStr(errCode),
					}).Debug("Failed to get image info")
					continue
				}

				image := share.CLUSImage{Domain: itf.Domain, Repo: itf.Repo, Tag: tag, RegMod: itf.RegMod}
				if exist, ok := imageMap[info.ID]; ok {
					exist.Add(image)
				} else {
					imageMap[info.ID] = utils.NewSet(image)
				}

				total++
			}
		}

		return imageMap, total, nil
	}
}

func (rs *Registry) checkAndPutImageResult(sctx *scanContext, id string, result *share.ScanResult, retAction scheduler.Action) int {
	rs.stateLock()
	defer rs.stateUnlock()

	if retAction != scheduler.TaskActionRequeue {
		rs.taskQueue.Remove(id)
	}

	// Scan might be stopped and registry might be removed, the task is canceled first, so
	// check the context and don't do anything if task is canceled.
	select {
	case <-sctx.ctx.Done():
		return -1
	default:
		// not canceled, continue
	}

	if sum, ok := rs.summary[id]; ok {
		sum.ScannedAt = time.Now().UTC()

		sum.Provider = result.Provider
		sum.BaseOS = result.Namespace
		sum.Version = result.Version
		sum.Result = result.Error
		if result.Error == share.ScanErrorCode_ScanErrNone {
			sum.Status = api.ScanStatusFinished
		} else if result.Error == share.ScanErrorCode_ScanErrNotSupport {
			sum.Status = api.ScanStatusFinished
		} else if retAction == scheduler.TaskActionRequeue {
			sum.Status = api.ScanStatusScheduled
		} else {
			sum.Status = api.ScanStatusFailed
		}
		sum.Author = result.Author
		sum.Size = result.Size

		if sum.Status == api.ScanStatusFinished {
			sum.ScanFlags |= share.ScanFlagCVE
			if len(result.Layers) != 0 {
				sum.ScanFlags |= share.ScanFlagLayers
			}
			if result.Secrets != nil {
				sum.ScanFlags |= share.ScanFlagFiles
			}
		}

		report := share.CLUSScanReport{
			ScannedAt:  sum.ScannedAt,
			ScanResult: *result,
		}

		if sum.Status == api.ScanStatusFinished {
			clusHelper.PutRegistryImageSummaryAndReport(rs.config.Name, id, sum, &report)

			if len(rs.summary) > api.ScanPersistImageMax+scanPersistImageExtra {
				rs.cleanupOldImages()
			}
		} else {
			clusHelper.PutRegistryImageSummary(rs.config.Name, id, sum)
		}
	}

	count := rs.taskQueue.Cardinality()
	if count == 0 && !sctx.scheduling && retAction != scheduler.TaskActionRequeue {
		// stopScan() will be called
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name}).Debug("Registry scan done")
		state := share.CLUSRegistryState{Status: api.RegistryStatusIdle, StartedAt: rs.state.StartedAt}
		clusHelper.PutRegistryState(rs.config.Name, &state)
	}

	return count
}

// Return if should continue scanning
func (rs *Registry) checkAndPutImageScanning(sctx *scanContext, task *regScanTask) *share.CLUSRegistryImageSummary {
	if !isScanner() {
		return nil
	}

	rs.stateLock()
	defer rs.stateUnlock()

	// Scan might be stopped and registry might be removed, the task is canceled first, so
	// check the context and don't do anything if task is canceled.
	select {
	case <-sctx.ctx.Done():
		return nil
	default:
		// not canceled, continue
	}

	id := task.imageID

	sum, ok := rs.summary[id]
	if !ok || len(sum.Images) == 0 {
		return nil
	}

	sum.Status = api.ScanStatusScanning
	clusHelper.PutRegistryImageSummary(rs.config.Name, id, sum)
	return sum
}

// Return if should continue scanning
func (rs *Registry) checkAndPutRegState(ctx context.Context, errMsg string) bool {
	rs.stateLock()
	defer rs.stateUnlock()

	// Scan might be stopped and registry might be removed, the task is canceled first, so
	// check the context and don't do anything if task is canceled.
	select {
	case <-ctx.Done():
		return false
	default:
		// not canceled, continue
	}

	if errMsg != "" {
		state := share.CLUSRegistryState{
			Status:    api.RegistryStatusIdle,
			ErrMsg:    errMsg,
			ErrDetail: rs.errDetail,
			StartedAt: rs.state.StartedAt,
		}
		clusHelper.PutRegistryState(rs.config.Name, &state)
		return false
	}

	return true
}

func imageUpdateCallback(name string, img *share.CLUSImage, add bool) {
	smd.scanLog.WithFields(log.Fields{"registry": name, "image": *img, "add": add}).Debug()

	if isScanner() {
		if add {
			reg, ok := regMapLookup(name)
			if ok {
				// Not to start go routine because there could be more images pushed at the same time.
				// Probably shouldn't run them parallelly.
				reg.imageScanAdd(img)
			}
		} else {
			reg, ok := regMapLookup(name)
			if ok {
				reg.imageScanDelete(img)
			}
		}
	}
}

func (rs *Registry) imageScanDelete(img *share.CLUSImage) {
	rs.stateLock()
	defer rs.stateUnlock()
	for _, sum := range rs.summary {
		for i, e := range sum.Images {
			if img.Domain == e.Domain && img.Repo == e.Repo && img.Tag == e.Tag {
				if len(sum.Images) == 1 {
					rs.cleanupOneImage(sum.ImageID)
					// rs.summary will be cleaned up when responding the key removal
				} else {
					sum.Images = append(sum.Images[:i], sum.Images[i+1:]...)
				}
				return
			}
		}
	}
}

// no lock
func (rs *Registry) imageScanAdd(img *share.CLUSImage) {
	repo := *img // clone img so it's value won't be modified
	repos := []*share.CLUSImage{&repo}
	tags := []string{img.Tag}

	var imageTagFilter *share.CLUSImage
	for _, filter := range rs.config.ParsedFilters {
		filteredRepos, _ := filterRepos(repos, filter, rs.config.CreaterDomains, 0)
		if len(filteredRepos) > 0 {
			filteredRepos[0].Tag = filter.Tag
			imageTagFilter = filteredRepos[0]
			break
		}
	}

	if imageTagFilter == nil {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": img}).Error("No repo match - ignored")
		return
	}

	filteredTags, err := filterTags(tags, imageTagFilter.Tag, 0)

	if err, _ := rs.backupDrv.Login(rs.config); err != nil {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "error": err}).Error()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	imageMap, err := getImageMeta(ctx, rs.backupDrv, imageTagFilter, filteredTags)
	cancel()

	rs.backupDrv.Logout(false)

	if err != nil {
		smd.scanLog.WithFields(log.Fields{"error": err}).Error("Get image meta fail")
		return
	}

	if len(imageMap) == 0 {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": img}).Error("No tag match - ignored")
		return
	}

	rs1, ok := regMapLookup(rs.config.Name)

	// Because lock was released, we need check registry still exist and same
	if ok && rs1 == rs {
		// Because we use 'state' to sync, checking sctx is not enough, as state might have changed but scanStop is not call yet.
		rs.stateLock()
		state := clusHelper.GetRegistryState(rs.config.Name)
		if state != nil && state.Status == api.RegistryStatusScanning && rs.sctx != nil {
			// Scanning, schedule the image
			rs.scheduleScanImagesOnDemand(rs.sctx, imageMap)
			smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": img}).Debug("New image scheduled")
		} else {
			// Not scanning, start scan
			state = &share.CLUSRegistryState{Status: api.RegistryStatusScanning, StartedAt: time.Now().Unix()}
			clusHelper.PutRegistryState(rs.config.Name, state)

			smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": img}).Debug("Start scan")
		}
		rs.stateUnlock()

	} else {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": img}).Error("Registry changed - ignored")
	}

	return
}

// no lock
func (rs *Registry) imageScanStart(sctx *scanContext) {
	smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name}).Debug("")

	imageMap, total, err := rs.getScanImages(sctx, rs.driver, false)
	if err == context.Canceled {
		return
	} else if err != nil && total == 0 {
		msg := registryErrMsgImage
		if strings.Contains(err.Error(), "UNAUTHORIZED") {
			msg = registryErrMsgAuth
		}
		rs.checkAndPutRegState(sctx.ctx, msg)
		return
	}

	if !rs.checkAndPutRegState(sctx.ctx, "") {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name}).Debug("Registry scan canceled")
		return
	}

	rs.stateLock()

	rs.cleanupImages(sctx, imageMap)
	// No need to cleanup taskQueue() because stopScan must be called in the path

	count := rs.taskQueue.Cardinality()
	if count == 0 {
		// stopScan() will be called
		state := share.CLUSRegistryState{Status: api.RegistryStatusIdle, StartedAt: rs.state.StartedAt}
		clusHelper.PutRegistryState(rs.config.Name, &state)
	}

	rs.stateUnlock()

	smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "count": count}).Debug("Registry scan starts")
}

// Lock protected
func (rs *Registry) startScan() {
	if sctx, err := rs.newScanContext(); err != nil {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "error": err}).Error()
		state := share.CLUSRegistryState{
			Status: api.RegistryStatusIdle, ErrMsg: registryErrMsgConnect,
			ErrDetail: rs.errDetail, StartedAt: rs.state.StartedAt,
		}
		clusHelper.PutRegistryState(rs.config.Name, &state)
	} else {
		rs.sctx = sctx
		go rs.imageScanStart(sctx)
	}
}

// Lock protected. Called when we becomes the scanner
func (rs *Registry) resumeScan() {
	if sctx, err := rs.newScanContext(); err != nil {
		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "error": err}).Error()
		state := share.CLUSRegistryState{
			Status: api.RegistryStatusIdle, ErrMsg: registryErrMsgConnect,
			ErrDetail: rs.errDetail, StartedAt: rs.state.StartedAt,
		}
		clusHelper.PutRegistryState(rs.config.Name, &state)
	} else {
		rs.sctx = sctx
		for id, sum := range rs.summary {
			if sum.Status == api.ScanStatusScheduled || sum.Status == api.ScanStatusScanning {
				sum.Status = api.ScanStatusScheduled
				clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)

				task := &regScanTask{sctx: sctx, reg: rs, imageID: sum.ImageID}
				regScher.AddTask(task, false)
				rs.taskQueue.Add(id)
			}
		}
	}
}

// Lock protected
func (rs *Registry) stopScan() {
	rs.sctx.cancel()
	rs.sctx = nil
	rs.driver.Logout(false)

	for id, sum := range rs.summary {
		if sum.Status == api.ScanStatusScheduled || sum.Status == api.ScanStatusScanning {
			// when an image is reevaluated during the periodical scan, the status is not changed unless
			// the image is going to be rescanned, so the logic here is correct.
			sum.Status = api.ScanStatusIdle
			sum.ScannedAt = time.Time{}
			sum.BaseOS = ""
			sum.Version = ""
			sum.Result = share.ScanErrorCode_ScanErrNone

			clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
			if regScher != nil {
				regScher.DeleteTask(id, scheduler.PriorityLow)
				rs.taskQueue.Remove(id)
			}
		}
	}

	smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name}).Debug("Registry scan stopped")
}

func (rs *Registry) cleanup() {
	// cleanup cluster, data and state
	clusHelper.DeleteRegistryKeys(rs.config.Name)
}

// Lock protected,
// Regularly remove old finished image and report from kv and pv
func (rs *Registry) cleanupOldImages() {
	finished := make([]*share.CLUSRegistryImageSummary, 0)
	for _, sum := range rs.summary {
		if sum.Status == api.ScanStatusFinished {
			finished = append(finished, sum)
		}
	}
	if len(finished) > api.ScanPersistImageMax {
		sort.Slice(finished, func(i, j int) bool { return finished[i].ScannedAt.After(finished[j].ScannedAt) })
		dels := finished[api.ScanPersistImageMax:]
		for _, sum := range dels {
			rs.cleanupOneImage(sum.ImageID)
			// rs.summary will be cleaned up when responding the key removal
		}
		log.WithFields(log.Fields{"count": len(dels)}).Info("Remove old images")
	}
}

// Lock protected
func (rs *Registry) cleanupOneImage(id string) {
	clusHelper.DeleteRegistryImageSummaryAndReport(rs.config.Name, id)
	// rs.summary will be cleaned up when responding the key removal
}

func (rs *Registry) cleanupImages(sctx *scanContext, imageMap map[string]utils.Set) {
	// remove the out-of-date repository
	for id, _ := range rs.summary {
		if _, ok := imageMap[id]; !ok {
			rs.cleanupOneImage(id)
		}
	}
}

// Lock protected
func (rs *Registry) removeImageWithDifferentID(meta *imageMeta) {
	for image := range meta.images.Iter() {
		if id, ok := rs.image2ID[image.(share.CLUSImage)]; ok && id != meta.id {
			// Found image name with different id, remove the old id->image map
			if sum, ok := rs.summary[id]; ok {
				smd.scanLog.WithFields(log.Fields{"id": id, "image": image.(share.CLUSImage)}).Debug("Remove obsolete image")
				if l := len(sum.Images); l == 1 {
					rs.cleanupOneImage(id)
				} else {
					for i, e := range sum.Images {
						if e == image {
							// Remove the old entry
							sum.Images[i] = sum.Images[l-1]
							sum.Images = sum.Images[:l-1]
							clusHelper.PutRegistryImageSummary(rs.config.Name, id, sum)
							break
						}
					}
				}
			}

			// image2ID on other controllers will be updated in image state update listener.
			delete(rs.image2ID, image.(share.CLUSImage))
		}
	}
}

func (rs *Registry) bSkipScanImage(sum *share.CLUSRegistryImageSummary) bool {
	// smd.scanLog.WithFields(log.Fields{"sum": sum, "config": rs.config, "dbv": smd.db.CVEDBVersion}).Debug("SCT")

	// has not succeeded before
	if sum.Status != api.ScanStatusFinished {
		return false
	}

	// rescan was set but db has been changed
	if rs.config.RescanImage && sum.Version != smd.db.CVEDBVersion {
		return false
	}

	if (sum.ScanFlags & share.ScanFlagCVE) == 0 {
		return false
	}
	if rs.config.ScanLayers && (sum.ScanFlags&share.ScanFlagLayers) == 0 {
		// no layered CVE reports
		return false
	}
	if !rs.config.DisableFiles && (sum.ScanFlags&share.ScanFlagFiles) == 0 {
		// no secrets report
		return false
	}

	return true
}

// Lock protected
func (rs *Registry) scheduleScanImagesOnDemand(sctx *scanContext, imageMap map[string]*imageMeta) {
	for _, meta := range imageMap {
		// Check if image with the same name exist
		rs.removeImageWithDifferentID(meta)

		imageChanged := false

		// put the repository into scheduler task
		sum, ok := rs.summary[meta.id]
		if ok {
			smd.scanLog.WithFields(log.Fields{
				"registry": rs.config.Name, "images": meta.images, "status": sum.Status,
			}).Debug("Scanned image")

			for image := range meta.images.Iter() {
				found := false
				// Play safe, check if there is a duplication
				for _, e := range sum.Images {
					if e == image.(share.CLUSImage) {
						found = true
						break
					}
				}
				if !found {
					sum.Images = append(sum.Images, image.(share.CLUSImage))
					imageChanged = true
				}
			}

			// Check the previous scan status, keep scanned-at unchanged
			if rs.bSkipScanImage(sum) {
				smd.scanLog.WithFields(log.Fields{
					"images": meta.images, "sum.Version": sum.Version, "CVEDBVersion": smd.db.CVEDBVersion, "changed": imageChanged,
				}).Debug("Skip scanned image")

				if imageChanged {
					clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
				}
				continue
			}

			if sum.Status == api.ScanStatusScheduled {
				smd.scanLog.WithFields(log.Fields{"images": meta.images}).Debug("Image already scheduled")
				if imageChanged {
					clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
				}
				continue
			}

			sum.Status = api.ScanStatusScheduled
			clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
		} else {
			sum = &share.CLUSRegistryImageSummary{
				ImageID:  meta.id,
				Registry: rs.config.Registry,
				RegName:  rs.config.Name,
				Digest:   meta.digest,
				// Signed:    meta.signed, [2019.Apr] comment out until we can accurately tell it
				RunAsRoot: meta.runAsRoot,
				Status:    api.ScanStatusScheduled,
			}
			sum.Images = make([]share.CLUSImage, 0, meta.images.Cardinality())
			for image := range meta.images.Iter() {
				sum.Images = append(sum.Images, image.(share.CLUSImage))
			}
			rs.summary[meta.id] = sum
			// update status in cluster
			clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
		}

		smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "images": meta.images}).Debug("Schedule image scan")

		task := &regScanTask{sctx: sctx, reg: rs, imageID: sum.ImageID}
		regScher.AddTask(task, false)
		rs.taskQueue.Add(meta.id)
	}
}

// Lock protected
func (rs *Registry) scheduleScanImages(
	sctx *scanContext, drv registryDriver, itfList []*share.CLUSImage, tagList [][]string,
) (map[string]utils.Set, int) {
	var total int
	imageMap := make(map[string]utils.Set)

	for i := 0; i < len(itfList); i++ {
		itf := itfList[i]
		tags := tagList[i]

		for _, tag := range tags {
			info, errCode := drv.GetImageMeta(sctx.ctx, itf.Domain, itf.Repo, tag)
			if errCode != share.ScanErrorCode_ScanErrNone {
				smd.scanLog.WithFields(log.Fields{
					"repo": itf, "tag": tag, "error": scanUtils.ScanErrorToStr(errCode),
				}).Debug("Failed to get image info")
				continue
			}

			total++
			newImage := false

			// Add to the map to be returned
			image := share.CLUSImage{Domain: itf.Domain, Repo: itf.Repo, Tag: tag, RegMod: itf.RegMod}
			if exist, ok := imageMap[info.ID]; ok {
				exist.Add(image)
			} else {
				newImage = true
				imageMap[info.ID] = utils.NewSet(image)
			}

			rs.stateLock()

			skip := false
			sum, ok := rs.summary[info.ID]
			if ok {
				smd.scanLog.WithFields(log.Fields{
					"registry": rs.config.Name, "image": image, "status": sum.Status,
				}).Debug("Scanned image")

				// Update image summary, remove previously scanned image but keep the meta such as last scan version
				imageChanged := false
				if newImage {
					sum.Images = []share.CLUSImage{image}
					imageChanged = true
				} else {
					found := false
					// Play safe, check if there is a duplication
					for _, e := range sum.Images {
						if e == image {
							found = true
							break
						}
					}
					if !found {
						sum.Images = append(sum.Images, image)
						imageChanged = true
					}
				}

				if !newImage {
					// if the image of the same ID has been processed in this batch, only update image list in summary
					if imageChanged {
						clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
					}
					skip = true
				} else if rs.bSkipScanImage(sum) {
					// Check the previous scan status, keep scanned-at unchanged
					smd.scanLog.WithFields(log.Fields{
						"image": image, "sum.Version": sum.Version, "CVEDBVersion": smd.db.CVEDBVersion, "changed": imageChanged,
					}).Debug("Skip scanned image")

					if imageChanged {
						clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
					}
					skip = true
				} else if sum.Status == api.ScanStatusScheduled {
					smd.scanLog.WithFields(log.Fields{"image": image}).Debug("Image already scheduled")
					if imageChanged {
						clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
					}
					skip = true
				} else {
					sum.Status = api.ScanStatusScheduled
					clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
				}
			} else {
				sum = &share.CLUSRegistryImageSummary{
					ImageID:  info.ID,
					Registry: rs.config.Registry,
					RegName:  rs.config.Name,
					Digest:   info.Digest,
					// Signed:    info.Signed, [2019.Apr] comment out until we can accurately tell it
					RunAsRoot: info.RunAsRoot,
					Status:    api.ScanStatusScheduled,
					Images:    []share.CLUSImage{image},
				}
				rs.summary[info.ID] = sum
				// update status in cluster
				clusHelper.PutRegistryImageSummary(rs.config.Name, sum.ImageID, sum)
			}

			if !skip {
				smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name, "image": image}).Debug("Schedule image scan")

				task := &regScanTask{sctx: sctx, reg: rs, imageID: info.ID}
				regScher.AddTask(task, false)
				rs.taskQueue.Add(info.ID)
			}
			rs.stateUnlock()
		}
	}

	return imageMap, total
}

func (rs *Registry) getConfig(acc *access.AccessControl) *api.RESTRegistry {
	reg := &api.RESTRegistry{
		Name:          rs.config.Name,
		Type:          rs.config.Type,
		Registry:      rs.config.Registry,
		Username:      rs.config.Username,
		Password:      rs.config.Password,
		AuthToken:     rs.config.AuthToken,
		AuthWithToken: rs.config.AuthWithToken,
		Filters:       rs.config.Filters,
		RescanImage:   rs.config.RescanImage,
		ScanLayers:    rs.config.ScanLayers,
		RepoLimit:     rs.config.RepoLimit,
		TagLimit:      rs.config.TagLimit,
		Schedule: api.RESTScanSchedule{
			Schedule: rs.config.Schedule,
			Interval: rs.config.PollPeriod,
		},
		GitlabApiUrl:       rs.config.GitlabApiUrl,
		GitlabPrivateToken: rs.config.GitlabPrivateToken,
		IBMCloudTokenURL:   rs.config.IBMCloudTokenURL,
		IBMCloudAccount:    rs.config.IBMCloudAccount,
	}
	if len(rs.config.Domains) != 0 {
		reg.Domains = rs.config.Domains
	} else {
		reg.Domains = rs.config.CreaterDomains
	}

	if rs.config.AwsKey != nil {
		reg.AwsKey = &api.RESTAWSAccountKey{
			ID:              rs.config.AwsKey.ID,
			AccessKeyID:     rs.config.AwsKey.AccessKeyID,
			SecretAccessKey: rs.config.AwsKey.SecretAccessKey,
			Region:          rs.config.AwsKey.Region,
		}
	}
	reg.JfrogMode = rs.config.JfrogMode
	reg.JfrogAQL = rs.config.JfrogAQL
	if rs.config.GcrKey != nil {
		reg.GcrKey = &api.RESTGCRKey{
			JsonKey: rs.config.GcrKey.JsonKey,
		}
	}

	return reg
}

func (rs *Registry) getConfigSummary(acc *access.AccessControl) *api.RESTRegistrySummary {
	var queue, finish, failed, scan int

	rs.stateLock()
	defer rs.stateUnlock()
	for _, sum := range rs.summary {
		if sum.Status == api.ScanStatusScheduled {
			queue += len(sum.Images)
		} else if sum.Status == api.ScanStatusScanning {
			scan += len(sum.Images)
		} else if sum.Status == api.ScanStatusFailed {
			failed += len(sum.Images)
		} else if sum.Status == api.ScanStatusFinished {
			finish += len(sum.Images)
		}
	}

	summary := &api.RESTRegistrySummary{
		RESTRegistry: *rs.getConfig(acc),
		Status:       rs.state.Status,
		ErrMsg:       rs.state.ErrMsg,
		ErrDetail:    rs.state.ErrDetail,
		RESTScanStatus: api.RESTScanStatus{
			Scanned:         finish,
			Scheduled:       queue,
			Scanning:        scan,
			Failed:          failed,
			CVEDBVersion:    smd.db.CVEDBVersion,
			CVEDBCreateTime: smd.db.CVEDBCreateTime,
		},
	}
	if rs.state.StartedAt != 0 {
		summary.StartedAt = api.RESTTimeString(time.Unix(rs.state.StartedAt, 0))
	}

	return summary
}

func (rs *Registry) polling(ctx context.Context) {
	smd.scanLog.WithFields(log.Fields{"PollPeriod": rs.config.PollPeriod}).Debug("")
	if rs.config.PollPeriod < api.ScanIntervalMin || rs.config.PollPeriod > api.ScanIntervalMax {
		smd.scanLog.WithFields(log.Fields{"PollPeriod": rs.config.PollPeriod}).Error("Polling interval out of range")
		return
	}
	ticker := time.Tick(time.Second * time.Duration(rs.config.PollPeriod))
	for {
		select {
		case <-ctx.Done():
			smd.scanLog.Debug("polling done")
			return
		case <-ticker:
			if isScanner() {
				rs.stateLock()
				state := clusHelper.GetRegistryState(rs.config.Name)
				if state == nil || state.Status != api.RegistryStatusScanning {
					smd.scanLog.WithFields(log.Fields{"registry": rs.config.Name}).Debug("Start polling images")
					state := &share.CLUSRegistryState{Status: api.RegistryStatusScanning, StartedAt: time.Now().Unix()}
					clusHelper.PutRegistryState(rs.config.Name, state)
				}
				rs.stateUnlock()
			}
		}
	}
}

// -- task

const maxRetry = 3

type regScanTask struct {
	sctx    *scanContext
	reg     *Registry
	imageID string
	retries int
	cancel  context.CancelFunc
}

func (t *regScanTask) Print(msg string) {
	smd.scanLog.WithFields(log.Fields{"image": t.imageID, "registry": t.reg.config.Name, "retry": t.retries}).Debug(msg)
}

func (t *regScanTask) Key() string {
	// The same image ID can appear in different registry, so registry name is prefixed.
	return fmt.Sprintf("%s@%s", t.reg.config.Name, t.imageID)
}

func (t *regScanTask) Priority() scheduler.Priority {
	return scheduler.PriorityLow
}

func (t *regScanTask) StartTimer() {
}

func (t *regScanTask) CancelTimer() {
}

func (t *regScanTask) Expire() {
}

func (t *regScanTask) Handler(scanner string) scheduler.Action {
	var result *share.ScanResult

	id := t.imageID

	sum := t.reg.checkAndPutImageScanning(t.sctx, t)
	if sum == nil {
		smd.scanLog.Debug("Scan skipped")
		return scheduler.TaskActionDone
	}

	go func() {
		ctx, cancel := context.WithTimeout(t.sctx.ctx, scanReqTimeout)
		defer cancel()

		smd.scanLog.WithFields(log.Fields{"scanner": scanner, "registry": t.reg.config.Name, "repo": sum.Images[0].Repo, "tag": sum.Images[0].Tag}).Debug("Scan start")
		result = t.reg.driver.ScanImage(scanner, ctx, sum.ImageID, sum.Digest, sum.Images[0].Repo, sum.Images[0].Tag)
		smd.scanLog.WithFields(log.Fields{"scanner": scanner, "images": sum.Images, "result": scanUtils.ScanErrorToStr(result.Error)}).Debug("Scan done")

		retAction := scheduler.TaskActionDone
		if (result.Error == share.ScanErrorCode_ScanErrTimeout ||
			result.Error == share.ScanErrorCode_ScanErrRegistryAPI ||
			result.Error == share.ScanErrorCode_ScanErrFileSystem ||
			result.Error == share.ScanErrorCode_ScanErrNetwork ||
			result.Error == share.ScanErrorCode_ScanErrContainerAPI) && t.retries < maxRetry {
			t.retries++
			retAction = scheduler.TaskActionRequeue
			smd.scanLog.WithFields(log.Fields{"scanner": scanner, "images": sum.Images, "retry": t.retries}).Debug("requeue")
		}

		regScher.TaskDone(t, retAction)

		if left := t.reg.checkAndPutImageResult(t.sctx, id, result, retAction); left < 0 {
			smd.scanLog.WithFields(log.Fields{"registry": t.reg.config.Registry}).Debug("Registry scan canceled")
		}
	}()

	return scheduler.TaskActionWait
}
