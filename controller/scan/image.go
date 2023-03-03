package scan

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/controller/api"
	"github.com/zhyocean/neuvector/controller/common"
	"github.com/zhyocean/neuvector/controller/resource"
	"github.com/zhyocean/neuvector/controller/rpc"
	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/httptrace"
	scanUtils "github.com/zhyocean/neuvector/share/scan"
	"github.com/zhyocean/neuvector/share/utils"
)

type imageMeta struct {
	id        string
	digest    string
	signed    bool
	runAsRoot bool
	images    utils.Set // share.CLUSImage
}

// --

var ibMutex sync.RWMutex
var imageBank map[share.CLUSImage]utils.Set = make(map[share.CLUSImage]utils.Set) // repo -> tags, no tag is filled in the key
var imageMetaBank map[share.CLUSImage]*scanUtils.ImageInfo = make(map[share.CLUSImage]*scanUtils.ImageInfo)
var subscribers map[string]utils.Set = make(map[string]utils.Set) // source -> reg. name

func (m *scanMethod) GetRegistryDebugImages(source string) []*api.RESTRegistryDebugImage {
	ibMutex.Lock()
	defer ibMutex.Unlock()

	imgs := make([]*api.RESTRegistryDebugImage, len(imageBank))
	i := 0
	for repo, tags := range imageBank {
		imgs[i] = &api.RESTRegistryDebugImage{
			Domain:     repo.Domain,
			Repository: repo.Repo,
			Tags:       make([]*api.RESTRegistryDebugImageTag, tags.Cardinality()),
		}
		t := 0
		for tag := range tags.Iter() {
			imgs[i].Tags[t] = &api.RESTRegistryDebugImageTag{
				Tag:    tag.(resource.ImageTag).Tag,
				Serial: tag.(resource.ImageTag).Serial,
			}
			t++
		}
		i++
	}

	return imgs
}

// Image identified by repo name and serial number (digest in OpenShift)
// Say now bank has image, A:1 (Repo+Tag A, Serial 1); Now the new update is, A:2, B:3
// => the diff is, A:2, B:3
// Image summary in the registry is organized by image ID. It's like,
// X: A:1 ==> Y: A:2, Z: B:3. X: A:1 need to be removed in this case.

func imageBankUpdate(img *resource.Image) {
	key := share.CLUSImage{Repo: img.Repo, Domain: img.Domain}
	newTags := utils.NewSetFromSliceKind(img.Tags)

	ibMutex.Lock()
	oldTags, _ := imageBank[key]
	imageBank[key] = newTags

	var moded, deled utils.Set
	if oldTags != nil {
		moded = newTags.Difference(oldTags)
		deled = oldTags.Difference(newTags)
	} else {
		moded = newTags
		deled = utils.NewSet()
	}

	for tag := range deled.Iter() {
		img := share.CLUSImage{Repo: img.Repo, Domain: img.Domain, Tag: tag.(resource.ImageTag).Tag}
		delete(imageMetaBank, img)
	}

	var subs utils.Set
	if v, ok := subscribers[api.RegistryImageSourceOpenShift]; ok {
		subs = v.Clone()
	} else {
		subs = utils.NewSet()
	}

	ibMutex.Unlock()

	for reg := range subs.Iter() {
		for tag := range moded.Iter() {
			key.Tag = tag.(resource.ImageTag).Tag
			imageUpdateCallback(reg.(string), &key, true)
		}
		for tag := range deled.Iter() {
			key.Tag = tag.(resource.ImageTag).Tag
			imageUpdateCallback(reg.(string), &key, false)
		}
	}
}

func imageBankDelete(img *resource.Image) {
	key := share.CLUSImage{Repo: img.Repo, Domain: img.Domain}

	ibMutex.Lock()
	oldTags, ok := imageBank[key]
	if ok {
		delete(imageBank, key)
	}

	var subs utils.Set
	if v, ok := subscribers[api.RegistryImageSourceOpenShift]; ok {
		subs = v.Clone()
	} else {
		subs = utils.NewSet()
	}

	ibMutex.Unlock()

	for reg := range subs.Iter() {
		for tag := range oldTags.Iter() {
			key.Tag = tag.(resource.ImageTag).Tag
			imageUpdateCallback(reg.(string), &key, false)
		}
	}
}

// !!! called with regMux locked
func registerImageBank(source, name string) {
	smd.scanLog.WithFields(log.Fields{"source": source, "registry": name}).Debug()

	ibMutex.Lock()
	defer ibMutex.Unlock()
	if subs, ok := subscribers[source]; !ok {
		subscribers[source] = utils.NewSet(name)
	} else {
		subs.Add(name)
	}
}

// !!! called with regMux locked
func deregisterImageBank(source, name string) {
	smd.scanLog.WithFields(log.Fields{"source": source, "registry": name}).Debug()

	ibMutex.Lock()
	defer ibMutex.Unlock()
	if subs, ok := subscribers[source]; ok {
		subs.Remove(name)
	}
}

// --

type registryDriver interface {
	Login(cfg *share.CLUSRegistryConfig) (error, string)
	Logout(force bool)
	GetRepoList(org, repo string, limit int) ([]*share.CLUSImage, error)
	GetTagList(doamin, repo, tag string) ([]string, error)
	GetAllImages() (map[share.CLUSImage][]string, error)
	GetImageMeta(ctx context.Context, domain, repo, tag string) (*scanUtils.ImageInfo, share.ScanErrorCode)
	ScanImage(scanner string, ctx context.Context, id, digest, repo, tag string) *share.ScanResult
	SetConfig(cfg *share.CLUSRegistryConfig)
	SetTracer(tracer httptrace.HTTPTrace)
	GetTracer() httptrace.HTTPTrace
}

type base struct {
	regURL      string
	rc          *scanUtils.RegClient
	username    string
	password    string
	proxy       string
	scanLayers  bool
	scanSecrets bool
	tracer      httptrace.HTTPTrace
}

func (r *base) url(pathTemplate string, args ...interface{}) string {
	pathSuffix := fmt.Sprintf(pathTemplate, args...)
	url := fmt.Sprintf("%s%s", r.regURL, pathSuffix)
	return url
}

func (r *base) newRegClient(url, username, password string) error {
	rc := scanUtils.NewRegClient(url, username, password, r.proxy, r.tracer)
	r.rc = rc
	r.username = username
	r.password = password
	return nil
}

func (r *base) SetConfig(cfg *share.CLUSRegistryConfig) {
	r.regURL = cfg.Registry
	r.scanLayers = cfg.ScanLayers
	r.scanSecrets = !cfg.DisableFiles
	r.proxy = GetProxy(cfg.Registry)
}

func (r *base) GetTracer() httptrace.HTTPTrace {
	return r.tracer
}

func (r *base) SetTracer(tracer httptrace.HTTPTrace) {
	r.tracer = tracer
}

func (r *base) Login(cfg *share.CLUSRegistryConfig) (error, string) {
	r.newRegClient(cfg.Registry, cfg.Username, cfg.Password)
	r.rc.Alive()
	return nil, ""
}

func (r *base) Logout(force bool) {
}

func (r *base) GetRepoList(org, name string, limit int) ([]*share.CLUSImage, error) {
	smd.scanLog.Debug()

	if !strings.Contains(name, "*") {
		if org == "" {
			return []*share.CLUSImage{&share.CLUSImage{Repo: name}}, nil
		} else {
			return []*share.CLUSImage{&share.CLUSImage{Repo: fmt.Sprintf("%s/%s", org, name)}}, nil
		}
	}

	repos, err := r.rc.Repositories()
	if err != nil {
		return nil, err
	}

	list := make([]*share.CLUSImage, len(repos))
	for i, repo := range repos {
		list[i] = &share.CLUSImage{Repo: repo}
	}
	return list, nil
}

func (r *base) GetTagList(domain, repo, tag string) ([]string, error) {
	smd.scanLog.Debug()

	if !strings.Contains(tag, "*") {
		return []string{tag}, nil
	}

	return r.rc.Tags(repo)
}

func (r *base) GetAllImages() (map[share.CLUSImage][]string, error) {
	return nil, common.ErrUnsupported
}

func (r *base) GetImageMeta(ctx context.Context, domain, repo, tag string) (*scanUtils.ImageInfo, share.ScanErrorCode) {
	rinfo, errCode := r.rc.GetImageInfo(ctx, repo, tag)
	return rinfo, errCode
}

func (r *base) ScanImage(scanner string, ctx context.Context, id, digest, repo, tag string) *share.ScanResult {
	req := &share.ScanImageRequest{
		Registry:    r.regURL,
		Username:    r.username,
		Password:    r.password,
		Repository:  repo,
		Tag:         tag,
		Proxy:       r.proxy,
		ScanLayers:  r.scanLayers,
		ScanSecrets: r.scanSecrets,
	}
	result, err := rpc.ScanImage(scanner, ctx, req)
	if result == nil || err != nil {
		// rpc request not made
		smd.scanLog.WithFields(log.Fields{"error": err}).Error()
		result = &share.ScanResult{Error: share.ScanErrorCode_ScanErrNetwork}
	}

	return result
}

// --

func filterRepos(repos []*share.CLUSImage, f *share.CLUSRegistryFilter, createrDomains []string, limit int) ([]*share.CLUSImage, error) {
	// Treat filter "*" (converted to .* already) specially, but Org being empty might not be acceptable
	// for some registries.
	matchAll := (f.Org == "" && f.Repo == ".*" && createrDomains == nil)

	var repoRegex *regexp.Regexp
	var err error
	matches := make([]*share.CLUSImage, 0)
	for _, r := range repos {
		if !matchAll {
			if repoRegex == nil {
				// already compiled when config
				// Treat repoFilter as regex, even it is normal text
				repoRegex, err = regexp.Compile(fmt.Sprintf("^%s$", f.Repo))
				if err != nil {
					return nil, err
				}
			}

			var org, name string
			name = r.Repo

			// if no organization provide, compare the whole repository
			if f.Org != "" || createrDomains != nil {
				if i := strings.Index(r.Repo, "/"); i > 0 {
					org = r.Repo[:i]
					name = r.Repo[i+1:]
				}
			}

			if f.Org != "" && f.Org != org {
				continue
			}
			// if creater domain is not empty (namespace user), the org must match one of the domains
			if createrDomains != nil {
				found := false
				for _, cd := range createrDomains {
					if cd == org {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			if !repoRegex.MatchString(name) {
				continue
			}

			if common.OEMIgnoreImageRepo(r) {
				continue
			}
		}

		matches = append(matches, r)

		if limit != 0 && len(matches) >= limit {
			break
		}
	}
	return matches, nil
}

func filterTags(tags []string, regex string, limit int) ([]string, error) {
	tagRegex, err := regexp.Compile(fmt.Sprintf("^%s$", regex))
	if err != nil {
		return nil, err
	}

	list := make([]string, 0, len(tags))
	for _, tag := range tags {
		if regex != ".*" && tagRegex != nil && !tagRegex.MatchString(tag) {
			continue
		}

		list = append(list, tag)

		if limit != 0 && len(list) >= limit {
			break
		}
	}

	return list, nil
}

func getImageMeta(ctx context.Context, drv registryDriver, itf *share.CLUSImage, tags []string) (map[string]*imageMeta, error) {
	m := make(map[string]*imageMeta, 0)
	for _, tag := range tags {
		info, errCode := drv.GetImageMeta(ctx, itf.Domain, itf.Repo, tag)
		if errCode == share.ScanErrorCode_ScanErrNone {
			img := share.CLUSImage{
				Domain: itf.Domain,
				Repo:   itf.Repo,
				Tag:    tag,
				RegMod: itf.RegMod,
			}
			if meta, ok := m[info.ID]; !ok {
				meta = &imageMeta{
					id:        info.ID,
					digest:    info.Digest,
					signed:    info.Signed,
					runAsRoot: info.RunAsRoot,
					images:    utils.NewSet(),
				}
				meta.images.Add(img)
				m[info.ID] = meta
			} else {
				meta.images.Add(img)
			}
		} else {
			smd.scanLog.WithFields(log.Fields{"error": scanUtils.ScanErrorToStr(errCode)}).Debug("Failed to get repository info")
		}

		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, context.Canceled
			default:
				// not canceled, continue
			}
		}
	}

	return m, nil
}
