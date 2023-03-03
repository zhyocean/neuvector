package rest

// #include "../../defs.h"
import "C"

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"

	"github.com/glenn-brown/golang-pkg-pcre/src/pkg/pcre"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"

	"github.com/neuvector/neuvector/controller/access"
	"github.com/neuvector/neuvector/controller/api"
	"github.com/neuvector/neuvector/controller/common"
	"github.com/neuvector/neuvector/controller/kv"
	"github.com/neuvector/neuvector/controller/rpc"
	"github.com/neuvector/neuvector/share"
)

func handlerDlpSensorList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	query := restParseQuery(r)

	var resp api.RESTDlpSensorsData
	resp.Sensors = make([]*api.RESTDlpSensor, 0)

	dlpsensors := cacher.GetAllDlpSensors(acc)
	// Filter
	if len(dlpsensors) <= query.start {
		restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp rule list")
		return
	}

	if len(dlpsensors) > 1 && len(query.sorts) > 0 {
		// Convert struct slice to interface slice
		var data []interface{} = make([]interface{}, len(dlpsensors))
		for i, d := range dlpsensors {
			data[i] = d
		}
		// Sort
		restNewSorter(data, query.sorts).Sort()
		for i, d := range data {
			dlpsensors[i] = d.(*api.RESTDlpSensor)
		}
	} else {
		sort.Slice(dlpsensors, func(i, j int) bool { return dlpsensors[i].Name < dlpsensors[j].Name })
	}

	if query.limit == 0 {
		resp.Sensors = dlpsensors[query.start:]
	} else {
		var end int
		if query.start+query.limit > len(dlpsensors) {
			end = len(dlpsensors)
		} else {
			end = query.start + query.limit
		}
		resp.Sensors = dlpsensors[query.start:end]
	}

	//always put predefined sensor in front
	preSensors := make([]*api.RESTDlpSensor, 0)
	userSensors := make([]*api.RESTDlpSensor, 0)
	for _, sen := range resp.Sensors {
		if sen.Predefine {
			preSensors = append(preSensors, sen)
		} else {
			userSensors = append(userSensors, sen)
		}
	}
	preSensors = append(preSensors, userSensors...)
	resp.Sensors = preSensors

	log.WithFields(log.Fields{"entries": len(resp.Sensors)}).Debug("Response")
	restRespSuccess(w, r, &resp, acc, login, nil, "Get all dlp sensors")
}

func handlerDlpRuleList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	query := restParseQuery(r)

	var resp api.RESTDlpRulesData
	resp.Rules = make([]*api.RESTDlpRule, 0)

	rules, err := cacher.GetDlpRules(acc)
	if rules == nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	// Filter
	if len(rules) <= query.start {
		restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp rule list")
		return
	}

	if len(rules) > 1 && len(query.sorts) > 0 {
		// Convert struct slice to interface slice
		var data []interface{} = make([]interface{}, len(rules))
		for i, d := range rules {
			data[i] = d
		}
		// Sort
		restNewSorter(data, query.sorts).Sort()
		for i, d := range data {
			rules[i] = d.(*api.RESTDlpRule)
		}
	} else {
		sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	}

	if query.limit == 0 {
		resp.Rules = rules[query.start:]
	} else {
		var end int
		if query.start+query.limit > len(rules) {
			end = len(rules)
		} else {
			end = query.start + query.limit
		}
		resp.Rules = rules[query.start:end]
	}
	log.WithFields(log.Fields{"entries": len(resp.Rules)}).Debug("Response")
	restRespSuccess(w, r, &resp, acc, login, nil, "Get all dlp rules")
}

func handlerDlpGroupList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	query := restParseQuery(r)

	var resp api.RESTDlpGroupsData
	resp.DlpGroups = make([]*api.RESTDlpGroup, 0)

	dlpgroups := cacher.GetAllDlpGroup(acc)
	// Filter
	if len(dlpgroups) <= query.start {
		restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp group list")
		return
	}

	if len(dlpgroups) > 1 && len(query.sorts) > 0 {
		// Convert struct slice to interface slice
		var data []interface{} = make([]interface{}, len(dlpgroups))
		for i, d := range dlpgroups {
			data[i] = d
		}
		// Sort
		restNewSorter(data, query.sorts).Sort()
		for i, d := range data {
			dlpgroups[i] = d.(*api.RESTDlpGroup)
		}
	} else {
		sort.Slice(dlpgroups, func(i, j int) bool { return dlpgroups[i].Name < dlpgroups[j].Name })
	}

	if query.limit == 0 {
		resp.DlpGroups = dlpgroups[query.start:]
	} else {
		var end int
		if query.start+query.limit > len(dlpgroups) {
			end = len(dlpgroups)
		} else {
			end = query.start + query.limit
		}
		resp.DlpGroups = dlpgroups[query.start:end]
	}
	log.WithFields(log.Fields{"entries": len(resp.DlpGroups)}).Debug("all dlp groups")
	restRespSuccess(w, r, &resp, acc, login, nil, "Get all dlp group")
}

func handlerDlpSensorShow(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	drsensor := ps.ByName("name")

	dlpsensor, err := cacher.GetDlpSensor(drsensor, acc)
	if dlpsensor == nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}
	resp := api.RESTDlpSensorData{Sensor: dlpsensor}
	restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp sensor detail")
}

func handlerDlpRuleShow(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	rule, err := cacher.GetDlpRule(name, acc)
	if rule == nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}
	resp := api.RESTDlpRuleData{Rule: rule}
	restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp rule detail")
}

func handlerDlpGroupShow(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	dgroup := ps.ByName("name")

	//Authorize group
	exist, err := cacher.DoesGroupExist(dgroup, acc)
	if !exist {
		log.WithFields(log.Fields{"group": dgroup}).Debug("Group does not exist!")
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	dlpgroup, err := cacher.GetDlpGroup(dgroup, acc)
	if dlpgroup == nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}
	resp := api.RESTDlpGroupData{DlpGroup: dlpgroup}
	restRespSuccess(w, r, &resp, acc, login, nil, "Get dlp group detail")
}

var regPattern *regexp.Regexp = regexp.MustCompile(`^\.*\*$`)

func validateDlpRuleConfig(list []api.RESTDlpRule) error {
	for _, rule := range list {
		if !isObjectNameValid(rule.Name) || len(rule.Name) >= api.DlpRuleNameMaxLen {
			log.WithFields(log.Fields{"name": rule.Name, "name_len": len(rule.Name)}).Error("Invalid rule name")
			return fmt.Errorf("dlp rule %s: invalid name format", rule.Name)
		}
		if len(rule.Patterns) == 0 {
			log.WithFields(log.Fields{"name": rule.Name}).Error("Dlp rule must have pattern")
			return fmt.Errorf("dlp rule %s: dlp rule must have pattern", rule.Name)
		}
		if len(rule.Patterns) > api.DlpRulePatternMaxNum {
			log.WithFields(log.Fields{"name": rule.Name, "num": len(rule.Patterns), "max": api.DlpRulePatternMaxNum}).Error("Dlp rule exceed max patterns")
			return fmt.Errorf("dlp rule %s: must have no more than %d patterns", rule.Name, api.DlpRulePatternMaxNum)
		}
		total_len := 0
		for _, pt := range rule.Patterns {
			if pt.Op == share.CriteriaOpRegex || pt.Op == share.CriteriaOpNotRegex {
				if len(pt.Value) > api.DlpRulePatternMaxLen || len(pt.Value) <= 0 {
					log.WithFields(log.Fields{"pattern": pt.Value, "pattern_len": len(pt.Value)}).Error("Invalid pattern length")
					return fmt.Errorf("dlp rule %s: invalid pattern length (%d)", rule.Name, len(pt.Value))
				}
				total_len += len(pt.Value)
				if total_len > api.DlpRulePatternTotalMaxLen {
					log.WithFields(log.Fields{"total": total_len, "max": api.DlpRulePatternTotalMaxLen}).Error("Exceed mac total pattern length")
					return fmt.Errorf("dlp rule %s: total pattern length %d exceed max allowed %d", rule.Name, total_len, api.DlpRulePatternTotalMaxLen)
				}
				if pt.Context != "" &&
					pt.Context != share.DlpPatternContextURI &&
					pt.Context != share.DlpPatternContextHEAD &&
					pt.Context != share.DlpPatternContextBODY &&
					pt.Context != share.DlpPatternContextPACKET {
					log.WithFields(log.Fields{"context": pt.Context}).Error("Invalid pattern context")
					return fmt.Errorf("dlp rule %s: invalid pattern context (%s)", rule.Name, pt.Context)
				}
				if _, err := pcre.Compile(pt.Value, 0); err != nil {
					log.WithFields(log.Fields{"error": err}).Error("Invalid regex in pattern criteria")
					return fmt.Errorf("dlp rule %s: invalid regex in pattern criteria (%s)", rule.Name, pt.Value)
				} else {
					if regPattern.MatchString(pt.Value) {
						log.WithFields(log.Fields{"error": err}).Error("Invalid regex in pattern criteria")
						return fmt.Errorf("dlp rule %s: invalid regex in pattern criteria (%s)", rule.Name, pt.Value)
					}
				}
			}
		}
	}
	return nil
}

var maxDlpRuleIDSeed int = 0

// return 0 if a unique id cannot be found
func getDlpRuleID(dlpsensor *share.CLUSDlpSensor) uint32 {
	var idx int = 0
	var maxid int = 0
	var rid int

	if maxDlpRuleIDSeed >= 0x7fffffff {
		log.Error("Reach the max dlp rule id seed")
		return 0
	}
	log.WithFields(log.Fields{"maxDlpRuleIDSeed": maxDlpRuleIDSeed}).Debug("")

	ids := make([]int, len(dlpsensor.RuleList))
	for _, cdr := range dlpsensor.RuleList {
		if cdr.ID < api.MinDlpRuleID {
			continue
		}
		ids[idx] = int(cdr.ID)
		if ids[idx] > maxid {
			maxid = ids[idx]
		}
		idx++
	}

	//each id use up one maxDlpRuleIDSeed count
	if maxDlpRuleIDSeed == 0 && maxid >= api.MinDlpRuleID {
		maxDlpRuleIDSeed = maxDlpRuleIDSeed + (maxid - api.MinDlpRuleID + 1)
	}

	rid = maxDlpRuleIDSeed%(api.MinDlpPredefinedRuleID-api.MinDlpRuleID-1) + api.MinDlpRuleID
	maxDlpRuleIDSeed++

	if rid > maxid {
		return uint32(rid)
	}

	sort.Ints(ids)
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if id != rid {
			return uint32(rid)
		} else {
			rid = id + 1
		}
	}
	if rid < api.MinDlpPredefinedRuleID {
		return uint32(rid)
	} else {
		return 0
	}
}

//lock is alreay hold when call this function
//clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
func CreatePredefaultSensor() {
	kv.CreateDefDlpRules(true)
	kv.CreatePreDlpSensor(true)
}

func handlerDlpSensorCreate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	// Read request
	body, _ := ioutil.ReadAll(r.Body)
	var rconf api.RESTDlpSensorConfigData
	err := json.Unmarshal(body, &rconf)
	if err != nil || rconf.Config == nil {
		log.WithFields(log.Fields{"error": err, "rconf": rconf}).Error("Request error")
		restRespError(w, http.StatusBadRequest, api.RESTErrInvalidRequest)
		return
	}

	conf := rconf.Config

	if !isObjectNameValid(conf.Name) {
		e := "Invalid characters in name"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidName, e)
		return
	}
	if conf.Name == share.CLUSDlpDefaultSensor {
		e := "Cannot create sensor with reserved name"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidName, e)
		return
	}

	if cached, err := cacher.DoesDlpSensorExist(conf.Name, acc); cached == true {
		e := "dlp sensor already exists"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrDuplicateName, e)
		return
	} else if err == common.ErrObjectAccessDenied {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	if conf.Comment != nil {
		if len(*conf.Comment) > api.DlpRuleCommentMaxLen {
			e := fmt.Sprintf("Comment exceed max %d characters!", api.DlpRuleCommentMaxLen)
			log.WithFields(log.Fields{"name": conf.Name, "comment_length": len(*conf.Comment)}).Error(e)
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
			return
		}
	}
	if conf.RuleChgList != nil || conf.RuleDelList != nil {
		e := "Cannot change or delete rules when creating sensor"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}
	if conf.Rules == nil {
		rules := make([]api.RESTDlpRule, 0)
		conf.Rules = &rules
	}
	if err := validateDlpRuleConfig(*conf.Rules); err != nil {
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, err.Error())
		return
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Acquire lock error")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	sensor := &share.CLUSDlpSensor{
		Name:          conf.Name,
		Groups:        make(map[string]string),
		RuleListNames: make(map[string]string),
		RuleList:      make(map[string]*share.CLUSDlpRule),
		PreRuleList:   make(map[string][]*share.CLUSDlpRule),
		Predefine:     false,
	}
	if conf.Comment != nil {
		sensor.Comment = *conf.Comment
	}
	//check user permission before creation
	if !acc.Authorize(sensor, nil) {
		e := "Object access denied"
		restRespErrorMessage(w, http.StatusForbidden, api.RESTErrObjectAccessDenied, e)
		return
	}

	var defsensor *share.CLUSDlpSensor
	defsensor = clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)

	/*
	* If the default/predefined dlp sensor is not
	* created in upgrading process, create it here.
	 */
	if defsensor == nil {
		CreatePredefaultSensor()
		defsensor = clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)
		if defsensor == nil {
			e := "sensor cannot be created in cluster!"
			log.WithFields(log.Fields{"sensor": sensor.Name}).Error(e)
			restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
			return
		}
		log.Debug("Creating predefined sensor!")
	}

	if defsensor.RuleList == nil {
		defsensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if defsensor.PreRuleList == nil {
		defsensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}

	for _, rdr := range *conf.Rules {
		rdr.Name = common.GetInternalDlpRuleName(rdr.Name, sensor.Name)
		cdr := share.CLUSDlpRule{
			Name: rdr.Name,
		}
		for _, rpt := range rdr.Patterns {
			cdr.Patterns = append(cdr.Patterns, share.CLUSDlpCriteriaEntry{
				Key:     rpt.Key,
				Value:   rpt.Value,
				Op:      rpt.Op,
				Context: rpt.Context,
			})
		}
		cdr.ID = getDlpRuleID(defsensor)
		if cdr.ID == 0 {
			e := "Dlp rule id overflow!"
			log.WithFields(log.Fields{"ID": cdr.ID}).Error(e)
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
			return
		}

		//save full rule with pattern in default sensor
		defsensor.RuleList[cdr.Name] = &cdr

		//new sensor use this rule, only save name
		sensor.RuleListNames[rdr.Name] = rdr.Name
	}
	//save full rule with pattern in default sensor
	clusHelper.PutDlpSensor(defsensor, false)

	//create new sensor
	clusHelper.PutDlpSensor(sensor, true)
	restRespSuccess(w, r, nil, acc, login, &rconf, "Create dlp sensor")
}

func handlerDlpRuleCreate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	// Read request
	body, _ := ioutil.ReadAll(r.Body)
	var rconf api.RESTDlpRuleConfigData
	err := json.Unmarshal(body, &rconf)
	if err != nil || rconf.Config == nil {
		log.WithFields(log.Fields{"error": err, "rconf": rconf}).Error("Request error")
		restRespError(w, http.StatusBadRequest, api.RESTErrInvalidRequest)
		return
	}

	conf := rconf.Config

	rules := make([]api.RESTDlpRule, 0)
	rdr := api.RESTDlpRule{
		Name:     conf.Name,
		Patterns: conf.Patterns,
	}
	rules = append(rules, rdr)

	if err := validateDlpRuleConfig(rules); err != nil {
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, err.Error())
		return
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Acquire lock error")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	sensor := clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)
	if sensor == nil {
		e := "default dlp sensor doesn't exist"
		log.WithFields(log.Fields{"name": share.CLUSDlpDefaultSensor}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}

	if sensor.RuleList == nil {
		sensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if sensor.PreRuleList == nil {
		sensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}
	prelist, foundInAllPre := sensor.PreRuleList[rdr.Name]
	if foundInAllPre && len(prelist) != 0 {
		e := "predefined rule with same name already exist!"
		log.WithFields(log.Fields{"rulename": rdr.Name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}
	_, foundInLocal := sensor.RuleList[rdr.Name]
	if foundInLocal {
		e := "rule with same name already exist!"
		log.WithFields(log.Fields{"rulename": rdr.Name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}
	cdr := share.CLUSDlpRule{
		Name: rdr.Name,
	}
	for _, rpt := range rdr.Patterns {
		cdr.Patterns = append(cdr.Patterns, share.CLUSDlpCriteriaEntry{
			Key:     rpt.Key,
			Value:   rpt.Value,
			Op:      rpt.Op,
			Context: rpt.Context,
		})
	}
	cdr.ID = getDlpRuleID(sensor)
	if cdr.ID == 0 {
		e := "Dlp rule id overflow!"
		log.WithFields(log.Fields{"ID": cdr.ID}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}
	sensor.RuleList[cdr.Name] = &cdr

	clusHelper.PutDlpSensor(sensor, false)
	restRespSuccess(w, r, nil, acc, login, &rconf, "Create dlp rule")
}

func handlerDlpSensorConfig(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	// Read request
	body, _ := ioutil.ReadAll(r.Body)
	var rconf api.RESTDlpSensorConfigData
	err := json.Unmarshal(body, &rconf)
	if err != nil || rconf.Config == nil {
		log.WithFields(log.Fields{"error": err, "rconf": rconf}).Error("Request error")
		restRespError(w, http.StatusBadRequest, api.RESTErrInvalidRequest)
		return
	}

	conf := rconf.Config

	if conf.Name == share.CLUSDlpDefaultSensor {
		e := "Cannot edit sensor with reserved name"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidName, e)
		return
	}

	if conf.Name == share.CLUSDlpCcSensor || conf.Name == share.CLUSDlpSsnSensor {
		e := "Cannot edit predefined sensor!"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}

	if conf.Name != name {
		e := "Name mismatch"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}

	if conf.Comment != nil {
		if len(*conf.Comment) > api.DlpRuleCommentMaxLen {
			e := fmt.Sprintf("Comment exceed max %d characters!", api.DlpRuleCommentMaxLen)
			log.WithFields(log.Fields{"name": conf.Name, "comment_length": len(*conf.Comment)}).Error(e)
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
			return
		}
	}

	if conf.Rules != nil {
		if err := validateDlpRuleConfig(*conf.Rules); err != nil {
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, err.Error())
			return
		}
	}

	if conf.RuleChgList != nil {
		if err := validateDlpRuleConfig(*conf.RuleChgList); err != nil {
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, err.Error())
			return
		}
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Acquire lock error")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	sensor := clusHelper.GetDlpSensor(name)
	if sensor == nil {
		e := "dlp sensor doesn't exist"
		log.WithFields(log.Fields{"name": name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	} else if !acc.Authorize(sensor, nil) {
		restRespAccessDenied(w, login)
		return
	}

	if conf.Comment != nil {
		sensor.Comment = *conf.Comment
	}

	if sensor.RuleListNames == nil {
		sensor.RuleListNames = make(map[string]string)
	}

	defsensor := clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)

	if defsensor.RuleList == nil {
		defsensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if defsensor.PreRuleList == nil {
		defsensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}

	if conf.Rules != nil { //used by GUI
		var newRuleListNames map[string]string = make(map[string]string)
		var delRuleListNames map[string]string = make(map[string]string)

		//newly created list
		for _, rdr := range *conf.Rules {
			rdr.Name = common.GetInternalDlpRuleName(rdr.Name, sensor.Name)
			newRuleListNames[rdr.Name] = rdr.Name
		}

		//list need to be deleted
		for _, rn := range sensor.RuleListNames { //old
			if _, ok := newRuleListNames[rn]; !ok { //not in new
				delRuleListNames[rn] = rn
			}
		}

		for _, rn := range delRuleListNames {
			_, foundInAll := defsensor.RuleList[rn]

			if foundInAll {
				delete(defsensor.RuleList, rn)
			}
			delete(sensor.RuleListNames, rn)
		}

		if sensor.RuleListNames == nil {
			sensor.RuleListNames = make(map[string]string)
		}

		for _, rdr := range *conf.Rules {
			rdr.Name = common.GetInternalDlpRuleName(rdr.Name, sensor.Name)
			//used by this sensor
			_, foundInLocal := sensor.RuleListNames[rdr.Name]
			//user created rule
			tcdr, foundInAll := defsensor.RuleList[rdr.Name]
			cdr := share.CLUSDlpRule{
				Name: rdr.Name,
			}
			for _, rpt := range rdr.Patterns {
				cdr.Patterns = append(cdr.Patterns, share.CLUSDlpCriteriaEntry{
					Key:     rpt.Key,
					Value:   rpt.Value,
					Op:      rpt.Op,
					Context: rpt.Context,
				})
			}
			if foundInLocal && foundInAll {
				cdr.ID = tcdr.ID
			} else {
				cdr.ID = getDlpRuleID(defsensor)
				if cdr.ID == 0 {
					e := "Dlp rule id overflow!"
					log.WithFields(log.Fields{"ID": cdr.ID}).Error(e)
					restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
					return
				}
			}
			//save full rule with pattern in default sensor
			defsensor.RuleList[cdr.Name] = &cdr
			//sensor use this rule, only save name
			sensor.RuleListNames[rdr.Name] = rdr.Name
		}
	} else { //used by CLI
		if conf.RuleDelList != nil {
			log.Debug("delete dlp rule list used by sensor!")
			for _, rdr := range *conf.RuleDelList {
				origname := rdr.Name
				rdr.Name = common.GetInternalDlpRuleName(rdr.Name, sensor.Name)
				//used by this sensor
				_, foundInLocal := sensor.RuleListNames[rdr.Name]
				//user created rule
				_, foundInAll := defsensor.RuleList[rdr.Name]

				if foundInLocal && foundInAll {
					delete(sensor.RuleListNames, rdr.Name)
					delete(defsensor.RuleList, rdr.Name)
				} else {
					//for upgrade, check rule name without DLPRuleTag
					//used by this sensor
					_, foundInLocal = sensor.RuleListNames[origname]
					//user created rule
					_, foundInAll = defsensor.RuleList[origname]
					if foundInLocal && foundInAll {
						delete(sensor.RuleListNames, origname)
						delete(defsensor.RuleList, origname)
					}
					if !foundInLocal {
						e := "Cannot find dlp rule in this sensor!"
						log.WithFields(log.Fields{"sensor": name, "rulename": rdr.Name}).Error(e)
						restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
						return
					}
					if !foundInAll {
						e := "Cannot find full dlp rule to delete!"
						log.WithFields(log.Fields{"sensor": defsensor.Name, "rulename": rdr.Name}).Error(e)
						restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
						return
					}
				}
			}
		}

		if conf.RuleChgList != nil {
			log.Debug("modify dlp rule list used by sensor!")
			for _, rdr := range *conf.RuleChgList {
				origname := rdr.Name
				rdr.Name = common.GetInternalDlpRuleName(rdr.Name, sensor.Name)
				//used by this sensor
				_, foundInLocal := sensor.RuleListNames[rdr.Name]
				//user created rule
				tcdr, foundInAll := defsensor.RuleList[rdr.Name]
				cdr := share.CLUSDlpRule{
					Name: rdr.Name,
				}
				for _, rpt := range rdr.Patterns {
					cdr.Patterns = append(cdr.Patterns, share.CLUSDlpCriteriaEntry{
						Key:     rpt.Key,
						Value:   rpt.Value,
						Op:      rpt.Op,
						Context: rpt.Context,
					})
				}
				if foundInLocal && foundInAll {
					cdr.ID = tcdr.ID
				} else {
					//for upgrade, check rule name without DLPRuleTag
					//used by this sensor
					_, foundInLocal = sensor.RuleListNames[origname]
					//user created rule
					_, foundInAll = defsensor.RuleList[origname]
					if foundInLocal && foundInAll {
						delete(sensor.RuleListNames, origname)
						delete(defsensor.RuleList, origname)
					}
					cdr.ID = getDlpRuleID(defsensor)
					if cdr.ID == 0 {
						e := "Dlp rule id overflow!"
						log.WithFields(log.Fields{"ID": cdr.ID}).Error(e)
						restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
						return
					}
				}
				//save full rule with pattern in default sensor
				defsensor.RuleList[cdr.Name] = &cdr
				//sensor use this rule, only save name
				sensor.RuleListNames[rdr.Name] = rdr.Name
			}
		}
	}
	clusHelper.PutDlpSensor(defsensor, false)
	clusHelper.PutDlpSensor(sensor, false)
	restRespSuccess(w, r, nil, acc, login, &rconf, "Configure dlp sensor")
}

func handlerDlpRuleConfig(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	// Read request
	body, _ := ioutil.ReadAll(r.Body)
	var rconf api.RESTDlpRuleConfigData
	err := json.Unmarshal(body, &rconf)
	if err != nil || rconf.Config == nil {
		log.WithFields(log.Fields{"error": err, "rconf": rconf}).Error("Request error")
		restRespError(w, http.StatusBadRequest, api.RESTErrInvalidRequest)
		return
	}

	conf := rconf.Config

	if conf.Name != name {
		e := "Name mismatch"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}

	rules := make([]api.RESTDlpRule, 0)
	rdr := api.RESTDlpRule{
		Name:     conf.Name,
		Patterns: conf.Patterns,
	}
	rules = append(rules, rdr)

	if err := validateDlpRuleConfig(rules); err != nil {
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, err.Error())
		return
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Acquire lock error")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	sensor := clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)
	if sensor == nil {
		e := "default dlp sensor doesn't exist"
		log.WithFields(log.Fields{"name": share.CLUSDlpDefaultSensor}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}

	//all dlp rule entries are saved inside sensor CLUSDlpDefaultSensor
	if sensor.RuleList == nil {
		sensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if sensor.PreRuleList == nil {
		sensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}
	prelist, foundInAllPre := sensor.PreRuleList[rdr.Name]
	if foundInAllPre && len(prelist) != 0 {
		e := "cannot modify predefined rule!"
		log.WithFields(log.Fields{"rulename": rdr.Name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}
	tcdr, foundInLocal := sensor.RuleList[rdr.Name]
	if !foundInLocal {
		e := "rule does not exist!"
		log.WithFields(log.Fields{"rulename": rdr.Name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}

	cdr := share.CLUSDlpRule{
		Name: rdr.Name,
	}
	for _, rpt := range rdr.Patterns {
		cdr.Patterns = append(cdr.Patterns, share.CLUSDlpCriteriaEntry{
			Key:     rpt.Key,
			Value:   rpt.Value,
			Op:      rpt.Op,
			Context: rpt.Context,
		})
	}
	cdr.ID = tcdr.ID
	sensor.RuleList[cdr.Name] = &cdr

	clusHelper.PutDlpSensor(sensor, false)
	restRespSuccess(w, r, nil, acc, login, &rconf, "Edit dlp rule")
}

func handlerDlpGroupConfig(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	// Read request
	body, _ := ioutil.ReadAll(r.Body)
	var rconf api.RESTDlpGroupConfigData
	err := json.Unmarshal(body, &rconf)
	if err != nil || rconf.Config == nil {
		log.WithFields(log.Fields{"error": err, "rconf": rconf}).Error("Request error")
		restRespError(w, http.StatusBadRequest, api.RESTErrInvalidRequest)
		return
	}

	conf := rconf.Config

	if conf.Name != name {
		e := "Name mismatch"
		log.WithFields(log.Fields{"name": conf.Name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}
	if cached, err := cacher.GetDlpGroup(conf.Name, acc); cached == nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	// Read from cluster
	cg := clusHelper.GetDlpGroup(name)
	if cg == nil {
		e := "Dlp group doesn't exist"
		log.WithFields(log.Fields{"name": name}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}

	// Apply dlp changes
	if conf.Status != nil {
		cg.Status = *conf.Status
	}

	if conf.RepSensors != nil { //used by GUI
		//delete existing sensor list used by dlp group
		cg.Sensors = make([]*share.CLUSDlpSetting, 0)

		//add replace sensor list to the dlp group
		if len(*conf.RepSensors) > 0 {
			for _, rs := range *conf.RepSensors {
				if rs.Name == share.CLUSDlpDefaultSensor {
					e := "Cannot use default sensor in dlp group!"
					log.WithFields(log.Fields{"name": rs.Name}).Error(e)
					restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
					return
				}
				if rs.Action != share.DlpRuleActionAllow && rs.Action != share.DlpRuleActionDrop {
					e := "Action is not supported!"
					log.WithFields(log.Fields{"sensor": rs}).Error(e)
					restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
					return
				}
				if clusHelper.GetDlpSensor(rs.Name) == nil {
					e := "DLP sensor does not exist"
					log.WithFields(log.Fields{"sensor": rs}).Error(e)
					restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
					return
				}
				cs := share.CLUSDlpSetting{Name: rs.Name, Action: rs.Action}
				if ret, ok := common.MergeDlpSensors(cg.Sensors, &cs); ok {
					cg.Sensors = ret
				}
			}
		}
	} else { //used by CLI
		if conf.DelSensors != nil {
			if cg.Sensors == nil {
				cg.Sensors = make([]*share.CLUSDlpSetting, 0)
			}
			if len(*conf.DelSensors) > 0 && len(cg.Sensors) > 0 {
				for _, rs := range *conf.DelSensors {
					var found bool = false
					cs := &share.CLUSDlpSetting{Name: rs, Action: share.DlpRuleActionDrop}
					idx, found := common.FindSensorInDlpGroup(cg.Sensors, cs)
					if found {
						cg.Sensors[idx] = nil
					} else {
						e := "Cannot find sensor to delete!"
						log.WithFields(log.Fields{"sensor": rs}).Error(e)
						//restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrObjectNotFound, e)
					}
				}

				list := make([]*share.CLUSDlpSetting, 0)
				for _, cs := range cg.Sensors {
					if cs != nil {
						list = append(list, cs)
					}
				}
				cg.Sensors = list
			}
		}

		if conf.Sensors != nil {
			if cg.Sensors == nil || len(*conf.Sensors) == 0 {
				cg.Sensors = make([]*share.CLUSDlpSetting, 0)
			}
			if len(*conf.Sensors) > 0 {
				for _, rs := range *conf.Sensors {
					if rs.Name == share.CLUSDlpDefaultSensor {
						e := "Cannot use default sensor!"
						log.WithFields(log.Fields{"name": rs.Name}).Error(e)
						restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
						return
					}
					if rs.Action != share.DlpRuleActionAllow && rs.Action != share.DlpRuleActionDrop {
						e := "Action not supported!"
						log.WithFields(log.Fields{"sensor": rs}).Error(e)
						restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
						return
					}
					if clusHelper.GetDlpSensor(rs.Name) == nil {
						e := "DLP sensor doesn't exist"
						log.WithFields(log.Fields{"sensor": rs}).Error(e)
						restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
						return
					}
					cs := share.CLUSDlpSetting{Name: rs.Name, Action: rs.Action}
					if ret, ok := common.MergeDlpSensors(cg.Sensors, &cs); ok {
						cg.Sensors = ret
					}

				}
			}
		}
	}

	// Write dlp group definition into key-value store
	if err := clusHelper.PutDlpGroup(cg, false); err != nil {
		log.WithFields(log.Fields{"error": err}).Error()
		restRespError(w, http.StatusInternalServerError, api.RESTErrFailWriteCluster)
		return
	}
	restRespSuccess(w, r, nil, acc, login, &rconf, "Configure dlp group")
}

func handlerDlpSensorDelete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	if name == share.CLUSDlpDefaultSensor {
		e := "Cannot delete default sensor!"
		log.WithFields(log.Fields{"name": name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}

	if name == share.CLUSDlpCcSensor || name == share.CLUSDlpSsnSensor {
		e := "Cannot delete predefined sensor!"
		log.WithFields(log.Fields{"name": name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrInvalidRequest, e)
		return
	}

	rdlpsensor, err := cacher.GetDlpSensor(name, acc)
	if rdlpsensor == nil {
		log.WithFields(log.Fields{"name": name}).Error("Fail to get sensor from cache!")
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	if len(rdlpsensor.GroupList) > 0 {
		e := "Sensor belong to group!"
		log.WithFields(log.Fields{"groups": (rdlpsensor.GroupList), "name": name}).Error(e)
		restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrObjectNotFound, e)
		return
	}

	//this is to check whether it is used by CLUSDlpGroup
	ingroup := cacher.DlpSensorInGroups(name)
	if ingroup {
		log.WithFields(log.Fields{"name": name}).Error("sensor belong to clusdlpgroup!")
		restRespError(w, http.StatusBadRequest, api.RESTErrObjectNotFound)
		return
	}

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Failed to acquire cluster lock")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	dlpsensor := clusHelper.GetDlpSensor(name)
	if dlpsensor == nil {
		log.WithFields(log.Fields{"name": name}).Error("Fail to get sensor!")
		restRespError(w, http.StatusBadRequest, api.RESTErrObjectNotFound)
		return
	}
	defsensor := clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)

	if defsensor.RuleList == nil {
		defsensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if defsensor.PreRuleList == nil {
		defsensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}
	for _, rn := range dlpsensor.RuleListNames {
		_, foundInAll := defsensor.RuleList[rn]

		if foundInAll {
			delete(defsensor.RuleList, rn)
		}
	}
	clusHelper.PutDlpSensor(defsensor, false)

	clusHelper.DeleteDlpSensor(name)

	restRespSuccess(w, r, nil, acc, login, nil, "Delete dlp sensor")
}

func handlerDlpRuleDelete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	}

	name := ps.ByName("name")

	lock, err := clusHelper.AcquireLock(share.CLUSLockPolicyKey, clusterLockWait)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("Acquire lock error")
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrFailLockCluster, err.Error())
		return
	}
	defer clusHelper.ReleaseLock(lock)

	sensor := clusHelper.GetDlpSensor(share.CLUSDlpDefaultSensor)
	if sensor == nil {
		e := "default dlp sensor doesn't exist"
		log.WithFields(log.Fields{"name": share.CLUSDlpDefaultSensor}).Error(e)
		restRespErrorMessage(w, http.StatusNotFound, api.RESTErrObjectNotFound, e)
		return
	}

	if sensor.RuleList == nil {
		sensor.RuleList = make(map[string]*share.CLUSDlpRule)
	}
	if sensor.PreRuleList == nil {
		sensor.PreRuleList = make(map[string][]*share.CLUSDlpRule)
	}
	_, found := sensor.RuleList[name]
	withSensor := cacher.IsDlpRuleUsedBySensor(name, acc)
	if found && !withSensor {
		delete(sensor.RuleList, name)
	} else {
		cdrlist, found := sensor.PreRuleList[name]
		if found && len(cdrlist) != 0 {
			e := "delete predefined rule not allowed!"
			log.WithFields(log.Fields{"rule": name}).Debug(e)
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrObjectNotFound, e)
		} else {
			e := "rule does not exist or is used by sensor(s)!"
			log.WithFields(log.Fields{"rule": name}).Debug(e)
			restRespErrorMessage(w, http.StatusBadRequest, api.RESTErrObjectNotFound, e)
		}
		return
	}
	clusHelper.PutDlpSensor(sensor, false)
	restRespSuccess(w, r, nil, acc, login, nil, "Delete dlp rule")
}

func dlpConvertToRestAction(act uint32) string {
	switch act {
	case C.DPI_ACTION_ALLOW:
		return share.DlpRuleActionAllow
	case C.DPI_ACTION_DROP:
		return share.DlpRuleActionDrop
	default:
		return share.DlpRuleActionAllow
	}
}

func derivedDlp2Rest(r *share.CLUSDerivedDlpRule) *api.RESTDlpSetting {
	p := &api.RESTDlpSetting{
		Name:   r.Name,
		Action: dlpConvertToRestAction(r.Action),
	}
	return p
}

func parseDerivedDlpRules(dlpRuleMap map[string]*share.CLUSDerivedDlpRuleArray,
	acc *access.AccessControl) []*api.RESTDerivedWorkloadDlpRule {

	wlrs := make([]*api.RESTDerivedWorkloadDlpRule, 0)
	for wlID, arr := range dlpRuleMap {
		var wl *api.RESTWorkloadBrief
		if wl, _ = cacher.GetWorkloadBrief(wlID, "", acc); wl == nil {
			continue
		}
		wlDlpRule := api.RESTDerivedWorkloadDlpRule{
			DlpWorkload: wl,
			Mode:        arr.Mode,
			DefAct:      arr.DefAct,
			ApplyDir:    arr.ApplyDir,
			DlpMacs:     make([]string, 0),
			DlpRules:    make([]*api.RESTDlpSetting, 0),
			WafRules:    make([]*api.RESTDlpSetting, 0),
			Rids:        make([]uint32, 0),
			Wafrids:     make([]uint32, 0),
			RuleType:    arr.RuleType,
		}
		for _, m := range arr.WlMacs {
			wlDlpRule.DlpMacs = append(wlDlpRule.DlpMacs, m)
		}

		for _, r := range arr.DlpRules {
			wlDlpRule.DlpRules = append(wlDlpRule.DlpRules, derivedDlp2Rest(r))
		}

		for _, r := range arr.WafRules {
			wlDlpRule.WafRules = append(wlDlpRule.WafRules, derivedDlp2Rest(r))
		}

		for _, r := range arr.Rids {
			wlDlpRule.Rids = append(wlDlpRule.Rids, r)
		}

		for _, r := range arr.Wafrids {
			wlDlpRule.Wafrids = append(wlDlpRule.Wafrids, r)
		}
		wlrs = append(wlrs, &wlDlpRule)
	}
	return wlrs
}

func handlerDebugDlpWlRuleList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	} else if !authDebugCaller(w, acc, login) {
		return
	}

	query := restParseQuery(r)

	agentID, wlID, err := getAgentWorkloadFromFilter(query.filters, acc)
	if err != nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	var filter share.CLUSFilter
	filter.Workload = wlID
	if dlpRules, err := rpc.GetDerivedDlpRules(agentID, &filter); err != nil {
		log.WithFields(log.Fields{"error": err}).Error()
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrClusterRPCError, "Failed to make the RPC call")
		return
	} else {
		resp := api.RESTDerivedWorkloadDlpRuleData{Rules: parseDerivedDlpRules(dlpRules.DlpRuleMap, acc)}
		restRespSuccess(w, r, &resp, acc, login, nil, "Get derived workload dlp rules")
	}
}

func parseDerivedDlpRuleEntries(dlpRuleEntries []*share.CLUSDerivedDlpRuleEntry,
	acc *access.AccessControl) []*api.RESTDerivedDlpRule {

	rdre := make([]*api.RESTDerivedDlpRule, len(dlpRuleEntries))
	for i, dre := range dlpRuleEntries {
		dlpRuleEntry := &api.RESTDerivedDlpRule{
			Name:     dre.Name,
			ID:       dre.ID,
			Patterns: make([]string, 0),
		}
		for _, p := range dre.Patterns {
			dlpRuleEntry.Patterns = append(dlpRuleEntry.Patterns, p)
		}

		rdre[i] = dlpRuleEntry
	}
	return rdre
}

func handlerDebugDlpRuleList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	} else if !authDebugCaller(w, acc, login) {
		return
	}

	query := restParseQuery(r)

	agentID, _, err := getAgentWorkloadFromFilter(query.filters, acc)
	if err != nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	var filter share.CLUSFilter
	if dlpRuleEntryArr, err := rpc.GetDerivedDlpRuleEntries(agentID, &filter); err != nil {
		log.WithFields(log.Fields{"error": err}).Error()
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrClusterRPCError, "Failed to make the RPC call")
		return
	} else {
		resp := api.RESTDerivedDlpRuleData{Rules: parseDerivedDlpRuleEntries(dlpRuleEntryArr.DlpRuleEntries, acc)}
		restRespSuccess(w, r, &resp, acc, login, nil, "Get derived workload dlp rules")
	}
}

func parseDerivedDlpRuleMacs(dlpRuleMacs []*share.CLUSDerivedDlpRuleMac,
	acc *access.AccessControl) []*api.RESTDerivedDlpRuleMac {
	log.Debug("")

	rmacs := make([]*api.RESTDerivedDlpRuleMac, len(dlpRuleMacs))
	for i, mac := range dlpRuleMacs {
		rdmac := &api.RESTDerivedDlpRuleMac{
			Mac: mac.Mac,
		}
		rmacs[i] = rdmac
	}
	return rmacs
}

func handlerDebugDlpRuleMac(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	log.WithFields(log.Fields{"URL": r.URL.String()}).Debug("")
	defer r.Body.Close()

	acc, login := getAccessControl(w, r, "")
	if acc == nil {
		return
	} else if !authDebugCaller(w, acc, login) {
		return
	}

	query := restParseQuery(r)

	agentID, _, err := getAgentWorkloadFromFilter(query.filters, acc)
	if err != nil {
		restRespNotFoundLogAccessDenied(w, login, err)
		return
	}

	var filter share.CLUSFilter
	if dlpRuleMacArr, err := rpc.GetDerivedDlpRuleMacs(agentID, &filter); err != nil {
		log.WithFields(log.Fields{"error": err}).Error()
		restRespErrorMessage(w, http.StatusInternalServerError, api.RESTErrClusterRPCError, "Failed to make the RPC call")
		return
	} else {
		resp := api.RESTDerivedDlpRuleMacData{Macs: parseDerivedDlpRuleMacs(dlpRuleMacArr.DlpRuleMacs, acc)}
		restRespSuccess(w, r, &resp, acc, login, nil, "Get derived dlp rule macs")
	}
}
