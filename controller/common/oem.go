package common

import (
	"github.com/zhyocean/neuvector/controller/api"
	"github.com/zhyocean/neuvector/share"
)

const OEMDefaultUserLocale string = "en"

var OEMClusterSecurityRuleGroup = "neuvector.com"
var OEMSecurityRuleGroup = "neuvector.com"

func OEMPlatformVersionURL() string {
	return ""
}

func OEMIgnoreWorkload(wl *share.CLUSWorkload) bool {
	if wl.Name == "curl" {
		return true
	}
	return false
}

func OEMIgnoreImageRepo(img *share.CLUSImage) bool {
	return false
}

func OEMLicenseValidate(info *api.RESTLicenseInfo) bool {
	return true
}
