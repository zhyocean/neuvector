package cache

import (
	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/controller/kv"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/cluster"
)

func PauseResumeStoreWatcher(fromCtrlerID, key string, action share.StoreWatcherAction) {
	log.WithFields(log.Fields{"fromCtrlerID ": fromCtrlerID, "action": action}).Info()
	if fromCtrlerID != cctx.LocalDev.Ctrler.ID {
		switch action {
		case share.StoreWatcherAction_PauseWatcher:
			kv.SetImporting(1)
			cluster.PauseWatcher(key)
		case share.StoreWatcherAction_ResumeWatcher:
			cluster.ResumeWatcher(key)
			kv.SetImporting(0)
		}
	}
}
