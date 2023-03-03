package fsmon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/agent/workerlet"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/global"
	"github.com/zhyocean/neuvector/share/osutil"
	"github.com/zhyocean/neuvector/share/utils"
)

////
var mLog *log.Logger = log.New()

const inodeChangeMask = syscall.IN_CLOSE_WRITE |
	syscall.IN_DELETE |
	syscall.IN_DELETE_SELF |
	syscall.IN_MOVE |
	syscall.IN_MOVE_SELF |
	syscall.IN_MOVED_TO

var packageFile utils.Set = utils.NewSet(
	"/var/lib/dpkg/status",
	"/var/lib/rpm/Packages",
	"/lib/apk/db/installed")

type SendAggregateReportCallback func(fsmsg *MonitorMessage) bool

var ImportantFiles []share.CLUSFileMonitorFilter = []share.CLUSFileMonitorFilter{
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/var/lib/dpkg/status", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/var/lib/rpm/Packages", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib/apk/db/installed", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/etc/hosts", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/etc/passwd", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/etc/shadow", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/etc/resolv\\.conf", Regex: ""},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/home/.*/\\.ssh", Regex: ".*"},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib", Regex: "ld-linux\\..*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib", Regex: "libc\\..*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib", Regex: "libpthread.*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib64", Regex: "ld-linux.*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib64", Regex: "libc\\..*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/lib64", Regex: "libpthread.*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/bin", Regex: ".*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/sbin", Regex: ".*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/usr/bin", Regex: ".*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/usr/sbin", Regex: ".*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/usr/local/bin", Regex: ".*", Recursive: true},
	share.CLUSFileMonitorFilter{Behavior: share.FileAccessBehaviorMonitor, Path: "/usr/local/sbin", Regex: ".*", Recursive: true},
}

var DefaultContainerConf share.CLUSFileMonitorProfile = share.CLUSFileMonitorProfile{
	Filters: ImportantFiles,
}

const (
	imonitorFileDelay = 10
)

const (
	fileEventAttr uint32 = (1 << iota)
	fileEventModified
	fileEventRemoved
	fileEventDirCreate
	fileEventSymModified
	fileEventReplaced
	fileEventDirRemoved
	fileEventAccessed
	fileEventDenied
)

var fileEventMsg = map[uint32]string{
	fileEventAttr:        "File attribute is changed.",
	fileEventModified:    "File was modified.",
	fileEventReplaced:    "File was replaced.",
	fileEventRemoved:     "File deleted from watched directory.",
	fileEventDirCreate:   "File created in watched directory.",
	fileEventSymModified: "File symlink was modified.",
	fileEventDirRemoved:  "Directory was deleted.",
	fileEventAccessed:    "File was accessed.",
	fileEventDenied:      "File access was denied.",
}

type SendFileAccessRuleCallback func(rules []*share.CLUSFileAccessRuleReq) error
type EstimateRuleSrcCallback func(id, path string, bBlocked bool) string

type fileMod struct {
	mask  uint32
	delay int
	finfo *osutil.FileInfoExt
	pInfo []*ProcInfo
}

type groupInfo struct {
	profile    *share.CLUSFileMonitorProfile
	mode       string
	applyRules map[string]utils.Set
	learnRules map[string]utils.Set
}

type FileWatch struct {
	mux        sync.Mutex
	aufs       bool
	fanotifier *FaNotify
	inotifier  *Inotify
	fileEvents map[string]*fileMod
	groups     map[int]*groupInfo
	sendrpt    SendAggregateReportCallback
	sendRule   SendFileAccessRuleCallback
	estRuleSrc EstimateRuleSrcCallback
	walkerTask *workerlet.Tasker
}

type MonitorMessage struct {
	ID        string
	Path      string
	Package   bool
	ProcName  string
	ProcPath  string
	ProcCmds  []string
	ProcPid   int
	ProcEUid  int
	ProcEUser string
	ProcPPid  int
	ProcPName string
	ProcPPath string
	Group     string
	Msg       string
	Count     int
	StartAt   time.Time
	Action    string
}

type ProcInfo struct {
	RootPid   int
	Name      string
	Path      string
	Cmds      []string
	Pid       int
	EUid      int
	EUser     string
	PPid      int
	PName     string
	PPath     string
	Deny      bool
	InProfile bool
}

type FaMonProbeData struct {
	NRoots    int
	NMntRoots int
	NDirMarks int
	NRules    int
	NPaths    int
	NDirs     int
}

type IMonProbeData struct {
	NWds   int
	NPaths int
	NDirs  int
}

type FmonProbeData struct {
	NFileEvents int
	NGroups     int
	Fan         FaMonProbeData
	Ino         IMonProbeData
}

type FsmonConfig struct {
	Profile *share.CLUSFileMonitorProfile
	Rule    *share.CLUSFileAccessRule
}

type FileMonitorConfig struct {
	IsAufs         bool
	EnableTrace    bool
	EndChan        chan bool
	WalkerTask     *workerlet.Tasker
	PidLookup      PidLookupCallback
	SendReport     SendAggregateReportCallback
	SendAccessRule SendFileAccessRuleCallback
	EstRule        EstimateRuleSrcCallback
}

func NewFileWatcher(config *FileMonitorConfig) (*FileWatch, error) {
	// for file monitor
	mLog.Out = os.Stdout
	mLog.Level = log.InfoLevel
	mLog.Formatter = &utils.LogFormatter{Module: "AGT"}
	if config.EnableTrace {
		mLog.SetLevel(log.DebugLevel)
	}

	n, err := NewFaNotify(config.EndChan, config.PidLookup, global.SYS)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Open fanotify fail")
		return nil, err
	}
	ni, err := NewInotify()
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Open inotify fail")
		return nil, err
	}

	go n.MonitorFileEvents()
	go ni.MonitorFileEvents()

	fw := &FileWatch{
		aufs:       config.IsAufs,
		fanotifier: n,
		inotifier:  ni,
		fileEvents: make(map[string]*fileMod),
		groups:     make(map[int]*groupInfo),
		sendrpt:    config.SendReport,
		sendRule:   config.SendAccessRule,
		estRuleSrc: config.EstRule,
		walkerTask: config.WalkerTask,
	}
	go fw.loop()
	return fw, nil
}

func (w *FileWatch) sendMsg(cid string, path string, event uint32, pInfo []*ProcInfo, mode string) {
	var eventMsg string

	// report the event by priority
	switch event {
	case fileEventDenied:
		eventMsg = fileEventMsg[fileEventDenied]
	case fileEventRemoved:
		eventMsg = fileEventMsg[fileEventRemoved]
	case fileEventDirRemoved:
		eventMsg = fileEventMsg[fileEventDirRemoved]
	case fileEventModified:
		eventMsg = fileEventMsg[fileEventModified]
	case fileEventDirCreate:
		eventMsg = fileEventMsg[fileEventDirCreate]
	case fileEventReplaced:
		eventMsg = fileEventMsg[fileEventReplaced]
	case fileEventAccessed:
		eventMsg = fileEventMsg[fileEventAccessed]
	case fileEventAttr:
		eventMsg = fileEventMsg[fileEventAttr]
	case fileEventSymModified:
		eventMsg = fileEventMsg[fileEventSymModified]
	}

	// log.WithFields(log.Fields{"path": path, "event": eventMsg, "proc": pInfo}).Debug("FMON:")

	if pInfo == nil {
		msg := MonitorMessage{
			ID:      cid,
			Path:    path,
			Group:   w.estRuleSrc(cid, path, event == fileEventDenied),
			Package: osutil.IsPackageLib(path),
			Msg:     eventMsg,
			Action:  share.PolicyActionViolate,
		}

		w.sendrpt(&msg)
		//	log.WithFields(log.Fields{"file": path, "container": cid}).Debug("File modified catched")
		return
	}
	// check whether the file was modified by same process.
	for i, pi := range pInfo {
		if i == 0 || !reflect.DeepEqual(pInfo[i-1], pi) {
			msg := MonitorMessage{
				ID:      cid,
				Path:    path,
				Group:   w.estRuleSrc(cid, path, event == fileEventDenied),
				Package: osutil.IsPackageLib(path),
				Msg:     eventMsg,
				Action:  share.PolicyActionViolate,
			}
			if pi != nil {
				msg.ProcName = pi.Name
				msg.ProcPath = pi.Path
				msg.ProcCmds = pi.Cmds
				msg.ProcPid = pi.Pid
				msg.ProcEUid = pi.EUid
				msg.ProcEUser = pi.EUser
				msg.ProcPPid = pi.PPid
				msg.ProcPName = pi.PName
				msg.ProcPPath = pi.PPath
				if pi.Deny {
					msg.Action = share.PolicyActionDeny
					msg.Msg = fileEventMsg[fileEventDenied]
				}
			}

			w.sendrpt(&msg)
			//	log.WithFields(log.Fields{"file": path, "container": cid}).Debug("File modified catched")
		} else {
			log.WithFields(log.Fields{"file": path, "container": cid, "pInfo": pi}).Debug("duplicate File modified")
		}
	}
}

func (w *FileWatch) loop() {
	msgTicker := time.Tick(time.Second * 4)
	// every 10s send learning rules to controller
	learnTicker := time.Tick(time.Second * 10)

	for {
		select {
		case <-msgTicker:
			w.HandleWatchedFiles()
		case <-learnTicker:
			w.reportLearningRules()
		}
	}
}

func (w *FileWatch) reportLearningRules() {
	learnRules := make([]*share.CLUSFileAccessRuleReq, 0)
	w.mux.Lock()
	for _, grp := range w.groups {
		if len(grp.learnRules) > 0 {
			for flt, rule := range grp.learnRules {
				for itr := range rule.Iter() {
					prf := itr.(string)
					rl := &share.CLUSFileAccessRuleReq{
						GroupName: grp.profile.Group,
						Filter:    flt,
						Path:      prf,
					}
					learnRules = append(learnRules, rl)
				}
			}
			grp.learnRules = make(map[string]utils.Set)
		}
	}
	w.mux.Unlock()
	if len(learnRules) > 0 {
		w.sendRule(learnRules)
	}
}

func filterIndexKey(filter share.CLUSFileMonitorFilter) string {
	return fmt.Sprintf("%s/%s", filter.Path, filter.Regex)
}

func (w *FileWatch) learnFromEvents(rootPid int, fmod *fileMod, path string, event uint32) {
	// log.WithFields(log.Fields{"path": path, "event": event}).Debug("FMON:")
	w.mux.Lock()
	grp, ok := w.groups[rootPid]
	if !ok {
		log.WithFields(log.Fields{"rootPid": rootPid}).Debug("FMON: group not found")
		w.mux.Unlock()
		return
	}
	mode := grp.mode
	if mode == share.PolicyModeLearn {
		flt := fmod.finfo.Filter.(*filterRegex)
		if applyRules, ok := grp.applyRules[flt.path]; ok {
			learnRules, ok := grp.learnRules[flt.path]
			if !ok {
				learnRules = utils.NewSet()
			}
			for _, pf := range fmod.pInfo {
				// only use the process name/path as profile
				if pf != nil && pf.Path != "" {
					if !applyRules.Contains(pf.Path) && !learnRules.Contains(pf.Path) {
						learnRules.Add(pf.Path)
						log.WithFields(log.Fields{"rule": pf.Path, "filter": flt}).Debug("FMON:")
					}
				}
			}
			// for inotify, cannot learn
			if learnRules.Cardinality() > 0 {
				grp.learnRules[flt.path] = learnRules
			}
		} else {
			log.WithFields(log.Fields{"path": path}).Debug("FMON: no access rules")
		}
	}
	w.mux.Unlock()

	if event != fileEventAccessed ||
		(mode == share.PolicyModeEnforce || mode == share.PolicyModeEvaluate) {
		w.sendMsg(fmod.finfo.ContainerId, path, event, fmod.pInfo, mode)
	}
}

func (w *FileWatch) UpdateAccessRules(name string, rootPid int, conf *share.CLUSFileAccessRule) {
	// log.WithFields(log.Fields{"name": name}).Debug("FMON:")
	w.mux.Lock()

	grp, ok := w.groups[rootPid]
	if !ok {
		log.WithFields(log.Fields{"name": name, "rules": conf}).Debug("FMON: Group not found")
		w.mux.Unlock()
		return
	}
	grp.applyRules = make(map[string]utils.Set)
	for idx, rule := range conf.Filters {
		if rule.CustomerAdd {
			applyRules := utils.NewSet()
			for _, app := range rule.Apps {
				applyRules.Add(app)
			}
			grp.applyRules[idx] = applyRules
		}
	}
	w.mux.Unlock()

	w.fanotifier.UpdateAccessRule(rootPid, conf)
}

func (w *FileWatch) Close() {
	log.Info()

	if w.fanotifier != nil {
		w.fanotifier.Close()
	}
	if w.inotifier != nil {
		w.inotifier.Close()
	}
}

func (w *FileWatch) cbNotify(filePath string, mask uint32, params interface{}, pInfo *ProcInfo) {
	//ignore the container remove event. they are too many
	if (mask&syscall.IN_IGNORED) != 0 || (mask&syscall.IN_UNMOUNT) != 0 {
		w.inotifier.RemoveMonitorFile(filePath)
		return
	}

	w.mux.Lock()
	defer w.mux.Unlock()
	if fm, ok := w.fileEvents[filePath]; ok {
		fm.mask |= mask
		fm.delay = 0
		fm.pInfo = append(fm.pInfo, pInfo)
	} else {
		pi := make([]*ProcInfo, 1)
		pi[0] = pInfo
		w.fileEvents[filePath] = &fileMod{
			mask:  mask,
			delay: 0,
			finfo: params.(*osutil.FileInfoExt),
			pInfo: pi,
		}
	}
}

func (w *FileWatch) addFile(finfo *osutil.FileInfoExt) {
	w.fanotifier.AddMonitorFile(finfo.Path, finfo.Filter, finfo.Protect, finfo.UserAdded, w.cbNotify, finfo)
	if _, path := global.SYS.ParseContainerFilePath(finfo.Path); packageFile.Contains(path) {
		w.inotifier.AddMonitorFile(finfo.Path, w.cbNotify, finfo)
	}
}

func (w *FileWatch) removeFile(fullpath string) {
	// w.fanotifier.RemoveMonitorFile(fullpath)		// should not
	if _, path := global.SYS.ParseContainerFilePath(fullpath); packageFile.Contains(path) {
		w.inotifier.RemoveMonitorFile(fullpath)
	}
}

func (w *FileWatch) addDir(finfo *osutil.FileInfoExt, files map[string]*osutil.FileInfoExt) {
	ff := make(map[string]interface{})
	for fpath, fi := range files {
		ff[fpath] = fi
	}
	w.fanotifier.AddMonitorDirFile(finfo.Path, finfo.Filter, finfo.Protect, finfo.UserAdded, ff, w.cbNotify, finfo)
}

func (w *FileWatch) getDirAndFileList(pid int, path, regx string, filter *filterRegex, recur, protect, userAdded bool,
	dirList map[string]*osutil.FileInfoExt) []*osutil.FileInfoExt {
	dirs, singles := w.getDirFileList(pid, path, regx, filter, recur, protect, userAdded)
	for _, di := range dirs {
		if diExist, ok := dirList[di.Path]; ok {
			diExist.Children = append(diExist.Children, di.Children...)
		} else {
			dirList[di.Path] = di
		}
	}
	return singles
}

func (w *FileWatch) getCoreFile(cid string, pid int, profile *share.CLUSFileMonitorProfile) (map[string]*osutil.FileInfoExt, []*osutil.FileInfoExt) {
	dirList := make(map[string]*osutil.FileInfoExt)
	singleFiles := make([]*osutil.FileInfoExt, 0)

	// get files and dirs from all filters
	for _, filter := range profile.Filters {
		flt := &filterRegex{path: filterIndexKey(filter)}
		flt.regex, _ = regexp.Compile(fmt.Sprintf("^%s$", flt.path))
		bBlockAccess := filter.Behavior == share.FileAccessBehaviorBlock
		bUserAdded := filter.CustomerAdd
		if strings.Contains(filter.Path, "*") {
			subDirs := w.getSubDirList(pid, filter.Path)
			for _, sub := range subDirs {
				singles := w.getDirAndFileList(pid, sub, filter.Regex, flt, filter.Recursive, bBlockAccess, bUserAdded, dirList)
				singleFiles = append(singleFiles, singles...)
			}
		} else {
			singles := w.getDirAndFileList(pid, filter.Path, filter.Regex, flt, filter.Recursive, bBlockAccess, bUserAdded, dirList)
			singleFiles = append(singleFiles, singles...)
		}
	}

	// get files and dirs from all filters
	for _, filter := range profile.FiltersCRD {
		flt := &filterRegex{path: filterIndexKey(filter)}
		flt.regex, _ = regexp.Compile(fmt.Sprintf("^%s$", flt.path))
		bBlockAccess := filter.Behavior == share.FileAccessBehaviorBlock
		bUserAdded := filter.CustomerAdd
		if strings.Contains(filter.Path, "*") {
			subDirs := w.getSubDirList(pid, filter.Path)
			for _, sub := range subDirs {
				singles := w.getDirAndFileList(pid, sub, filter.Regex, flt, filter.Recursive, bBlockAccess, bUserAdded, dirList)
				singleFiles = append(singleFiles, singles...)
			}
		} else {
			singles := w.getDirAndFileList(pid, filter.Path, filter.Regex, flt, filter.Recursive, bBlockAccess, bUserAdded, dirList)
			singleFiles = append(singleFiles, singles...)
		}
	}
	return dirList, singleFiles
}

//
func isRunTimeAddedFile(path string) bool {
	return strings.HasSuffix(path, "/root/etc/hosts") ||
		strings.HasSuffix(path, "/root/etc/hostname") ||
		strings.HasSuffix(path, "/root/etc/resolv.conf")
}

func (w *FileWatch) addCoreFile(cid string, dirList map[string]*osutil.FileInfoExt, singleFiles []*osutil.FileInfoExt) {
	// add files
	for _, finfo := range singleFiles {
		// need to move the cross link files to dirs
		di, ok := dirList[filepath.Dir(finfo.Path)]
		if ok && !isRunTimeAddedFile(finfo.Path) {
			finfo.Filter = di.Filter
			di.Children = append(di.Children, finfo)
		} else {
			finfo.ContainerId = cid
			w.addFile(finfo)
		}
	}

	// add directories
	for _, dir := range dirList {
		if dir == nil {
			continue
		}
		files := make(map[string]*osutil.FileInfoExt)
		for _, file := range dir.Children {
			if file == nil {
				continue
			}
			file.ContainerId = cid
			files[filepath.Base(file.Path)] = file
		}
		dir.ContainerId = cid
		w.addDir(dir, files)
	}
}

func (w *FileWatch) StartWatch(id string, rootPid int, conf *FsmonConfig, capBlock, bNeuvectorSvc bool) {
	log.WithFields(log.Fields{"id": id, "group": conf.Profile.Group, "Pid": rootPid, "mode": conf.Profile.Mode}).Debug("FMON:")
	// log.WithFields(log.Fields{"File": conf.Profile}).Debug("FMON:")
	// log.WithFields(log.Fields{"Access": conf.Rule}).Debug("FMON:")
	//// no access rules for neuvector and host
	if !osutil.IsPidValid(rootPid) {
		log.WithFields(log.Fields{"id": id, "Pid": rootPid}).Error("FMON: invalid Pid")
		return
	}

	if conf.Profile.Mode == "" {
		conf.Profile.Mode = share.PolicyModeLearn
	}
	var access, perm bool
	if conf.Profile.Mode == share.PolicyModeEnforce && !w.aufs && capBlock { // system containers will be limited at monitor mode
		perm = true
	} else {
		if rootPid == 1 || bNeuvectorSvc {
			// skip learn host and our container. only notify on modified
			access = false
		} else {
			if conf.Profile.Mode == share.PolicyModeLearn { // only for discover mode
				access = true
			}
		}
	}
	dirs, files := w.getCoreFile(id, rootPid, conf.Profile)

	w.fanotifier.SetMode(rootPid, access, perm, capBlock, bNeuvectorSvc)

	w.addCoreFile(id, dirs, files)

	w.fanotifier.StartMonitor(rootPid)

	w.mux.Lock()
	grp, ok := w.groups[rootPid]
	if !ok {
		grp = &groupInfo{
			learnRules: make(map[string]utils.Set),
			applyRules: make(map[string]utils.Set),
		}
		w.groups[rootPid] = grp
	}
	grp.profile = conf.Profile
	grp.mode = conf.Profile.Mode
	w.mux.Unlock()

	//// no access rules for neuvector and host
	if bNeuvectorSvc || rootPid == 1 {
		return
	}

	if conf.Rule != nil {
		w.UpdateAccessRules(conf.Profile.Group, rootPid, conf.Rule)
	}
}

func (w *FileWatch) AddProcessFile(id string, rootPid int, pid int) {
	if files := osutil.GetFileInfoExtFromPid(rootPid, pid); files != nil {
		for _, finfo := range files {
			finfo.ContainerId = id
			w.addFile(finfo)
		}
	}
}

func (w *FileWatch) HandleWatchedFiles() {
	events := make(map[string]*fileMod)
	w.mux.Lock()
	for filePath, fmod := range w.fileEvents {
		events[filePath] = fmod
		delete(w.fileEvents, filePath)
	}
	w.mux.Unlock()

	for fullPath, fmod := range events {
		pid, path := global.SYS.ParseContainerFilePath(fullPath)
		//to avoid false alarm of /etc/hosts and /etc/resolv.conf, check whether the container is still exist
		//these two files has attribute changed when the container leave
		//this maybe miss some events file changed right before container leave. But for these kind of event,
		//it is not useful if the container already leave
		//	log.WithFields(log.Fields{"pid": pid, "path": path, "pInfo": fmod.pInfo[0], "fInfo": fmod.finfo}).Debug("FMON:")
		//	if fmod.pInfo != nil {
		//		log.WithFields(log.Fields{"pInfo": fmod.pInfo[0]}).Debug("FMON:")
		//	}
		rootPath := global.SYS.ContainerProcFilePath(pid, "")
		if _, err := os.Stat(rootPath); err == nil && path != "" {
			var event uint32
			info, _ := os.Lstat(fullPath)
			if fmod.finfo.FileMode.IsDir() {
				event = w.handleDirEvents(fmod, info, fullPath, path, pid)
			} else {
				event = w.handleFileEvents(fmod, info, fullPath, pid)
			}
			if event != 0 {
				w.learnFromEvents(pid, fmod, path, event)
			}
		}
	}
}

func (w *FileWatch) handleDirEvents(fmod *fileMod, info os.FileInfo, fullPath, path string, pid int) uint32 {
	var event uint32
	// handle files inside directory
	if info != nil {
		// for new create file
		if (fmod.mask & (syscall.IN_MOVED_TO | syscall.IN_CREATE)) > 0 {
			event = fileEventDirCreate
			// add the new file to monitor
			dirFiles := make(map[string]*osutil.FileInfoExt)
			realPath := global.SYS.ContainerFilePath(pid, path)
			if files := osutil.GetFileInfoExtFromPath(pid, realPath, fmod.finfo.Filter, fmod.finfo.Protect, fmod.finfo.UserAdded); files != nil {
				for _, file := range files {
					file.ContainerId = fmod.finfo.ContainerId
					dirFiles[filepath.Base(path)] = file
				}
			}
			w.addDir(fmod.finfo, dirFiles)
		} else if (fmod.mask & syscall.IN_ACCESS) > 0 {
			event = fileEventAccessed
		} else {
			log.WithFields(log.Fields{"fullPath": fullPath}).Debug("directory event not found")
		}
	} else {
		// the path is itself means the directory was removed
		if fullPath == fmod.finfo.Path {
			event = fileEventDirRemoved
			w.fanotifier.RemoveMonitorFile(fullPath)
		} else {
			event = fileEventRemoved
		}
	}
	return event
}

func (w *FileWatch) handleFileEvents(fmod *fileMod, info os.FileInfo, fullPath string, pid int) uint32 {
	var event uint32
	if info != nil {
		if info.Mode() != fmod.finfo.FileMode {
			//attribute is changed
			event = fileEventAttr
			fmod.finfo.FileMode = info.Mode()
		}
		// check the hash existing and match
		// skip directory new file event, report later
		hash, err := osutil.GetFileHash(fullPath)
		if err != nil && !osutil.HashZero(fmod.finfo.Hash) ||
			err == nil && hash != fmod.finfo.Hash ||
			fmod.finfo.Size != info.Size() {
			event |= fileEventModified
			fmod.finfo.Hash = hash
		} else if (fmod.mask & syscall.IN_ACCESS) > 0 {
			event |= fileEventAccessed
		}
		if (fmod.finfo.FileMode & os.ModeSymlink) != 0 {
			//handle symlink
			rpath, err := osutil.GetContainerRealFilePath(pid, fullPath)
			if err == nil && fmod.finfo.Link != rpath {
				event |= fileEventSymModified
			}
		}
		if (fmod.mask & inodeChangeMask) > 0 {
			w.removeFile(fullPath)
			w.addFile(fmod.finfo)
		}
	} else {
		//file is removed
		event = fileEventRemoved
		w.fanotifier.RemoveMonitorFile(fullPath)
	}
	return event
}

func (w *FileWatch) ContainerCleanup(rootPid int) {
	w.fanotifier.ContainerCleanup(rootPid)
	w.inotifier.ContainerCleanup(rootPid)
	w.mux.Lock()
	defer w.mux.Unlock()
	delete(w.groups, rootPid)
}

func (w *FileWatch) GetWatchFileList(rootPid int) []*share.CLUSFileMonitorFile {
	return w.fanotifier.GetWatchFileList(rootPid)
}

func (w *FileWatch) GetAllFileMonitorFile() []*share.CLUSFileMonitorFile {
	return w.fanotifier.GetWatches()
}

////////
func (w *FileWatch) GetProbeData() *FmonProbeData {
	var probeData FmonProbeData

	w.mux.Lock()
	probeData.NFileEvents = len(w.fileEvents)
	probeData.NGroups = len(w.groups)
	w.mux.Unlock()

	if w.fanotifier != nil {
		w.fanotifier.GetProbeData(&probeData.Fan)
	}

	if w.inotifier != nil {
		w.inotifier.GetProbeData(&probeData.Ino)
	}

	return &probeData
}

func (w *FileWatch) SetMonitorTrace(bEnable bool) {
	if bEnable {
		mLog.Level = log.DebugLevel
	} else {
		mLog.Level = log.InfoLevel
	}
}

//////////////////////
const (
	dirIterTimeout  = time.Second * 4
	rootIterTimeout = time.Second * 16
)

// generic get a directory file list
func (w *FileWatch) getDirFileList(pid int, base, regexStr string, flt interface{}, recur, protect, userAdded bool) (map[string]*osutil.FileInfoExt, []*osutil.FileInfoExt) {
	dirList := make(map[string]*osutil.FileInfoExt)
	singleFiles := make([]*osutil.FileInfoExt, 0)

	tmOut := dirIterTimeout
	if base == "" {
		base += "/"
		tmOut = rootIterTimeout
	}
	base = strings.Replace(base, "\\.", ".", -1)
	dirs := utils.NewSet(base)

	// for recursive directory
	for dirs.Cardinality() > 0 {
		any := dirs.Any()
		absPath := any.(string)
		realPath := global.SYS.ContainerFilePath(pid, absPath)
		finfo, err := os.Stat(realPath)
		if err != nil {
			dirs.Remove(any)
			continue
		}

		// the path in the filter is single file
		if !finfo.IsDir() {
			if files := osutil.GetFileInfoExtFromPath(pid, realPath, flt, protect, userAdded); files != nil {
				// file and it's possible link
				singleFiles = append(singleFiles, files...)
			}
			dirs.Remove(any)
			continue
		}

		// directory and its files
		dirInfo := &osutil.FileInfoExt{
			FileMode:  finfo.Mode(),
			Path:      realPath,
			Filter:    flt,
			Protect:   protect,
			UserAdded: userAdded,
		}
		dirList[realPath] = dirInfo

		// log.WithFields(log.Fields{"realPath": realPath, "absPath": absPath}).Debug()
		res := workerlet.WalkPathResult{
			Dirs:  make([]*workerlet.DirData, 0),
			Files: make([]*workerlet.FileData, 0),
		}

		req := workerlet.WalkPathRequest{
			Pid:     pid,
			Path:    absPath,
			Timeout: tmOut,
		}

		bytesValue, _, err := w.walkerTask.Run(req)
		if err == nil {
			err = json.Unmarshal(bytesValue, &res)
		}

		if err != nil {
			log.WithFields(log.Fields{"path": base, "error": err}).Error()
		}

		for _, d := range res.Dirs {
			path := filepath.Join(realPath, d.Dir)
			if recur && realPath != path && regexStr == ".*" {
				// log.WithFields(log.Fields{"dir": path}).Debug()
				dinfo := &osutil.FileInfoExt{
					FileMode:  finfo.Mode(), // ??
					Path:      path,
					Filter:    flt,
					Protect:   protect,
					UserAdded: userAdded,
				}
				dirList[path] = dinfo
			}
		}

		for _, f := range res.Files {
			path := filepath.Join(realPath, f.File)
			if !recur && realPath != filepath.Dir(path) {
				continue
			}

			fstr := fmt.Sprintf("%s/%s", filepath.Dir(path), regexStr)
			regx, err := regexp.Compile(fmt.Sprintf("^%s$", fstr))
			if err != nil {
				log.WithFields(log.Fields{"error": err, "str": fstr}).Debug("regexp parse fail")
				continue
			}

			if regx.MatchString(path) {
				// log.WithFields(log.Fields{"path": path}).Debug()
				if files := osutil.GetFileInfoExtFromPath(pid, path, flt, protect, userAdded); files != nil {
					// check whether the files are in the directory, some file link to other position
					for _, file := range files {
						singleFiles = append(singleFiles, file)
						dirPath := filepath.Dir(file.Path)
						if di, ok := dirList[dirPath]; ok {
							di.Children = append(di.Children, file)
						} else {
							singleFiles = append(singleFiles, file)
						}
					}
				}
			}
		}
		dirs.Remove(any)
	}
	return dirList, singleFiles
}

func (w *FileWatch) getSubDirList(pid int, base string) []string {
	dirList := make([]string, 0)
	fstr := global.SYS.ContainerFilePath(pid, base)
	regxDir, err := regexp.Compile(fstr)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "str": fstr}).Debug("directory regexp parse fail")
		return dirList
	}
	baseStr := strings.Split(base, "/")
	var startDir string
	for i, dd := range baseStr {
		if strings.Contains(dd, "*") {
			break
		}
		if i > 0 {
			startDir += "/" + dd
		}
	}
	basePath := global.SYS.ContainerFilePath(pid, "")
	realPath := global.SYS.ContainerFilePath(pid, startDir)

	// log.WithFields(log.Fields{"startDir": startDir, "realPath": realPath, "basePath": basePath}).Debug()
	res := workerlet.WalkPathResult{
		Dirs:  make([]*workerlet.DirData, 0),
		Files: make([]*workerlet.FileData, 0),
	}

	req := workerlet.WalkPathRequest{
		Pid:     pid,
		Path:    startDir,
		Timeout: dirIterTimeout,
	}

	bytesValue, _, err := w.walkerTask.Run(req)
	if err == nil {
		err = json.Unmarshal(bytesValue, &res)
	}

	if err != nil {
		log.WithFields(log.Fields{"path": startDir, "error": err}).Error()
	}

	for _, d := range res.Dirs {
		path := filepath.Join(realPath, d.Dir)
		if regxDir.MatchString(path) {
			absPath := path[len(basePath):]
			// log.WithFields(log.Fields{"absPath": absPath, "path": path}).Debug()
			dirList = append(dirList, absPath)
		}
	}
	return dirList
}
