package scan

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/utils"
)

const gitTimeout = 20 * time.Second
const apiVersion = "api/v4"

type gitlab struct {
	base
	apiUrl       string
	privateToken string
	gitlabClient *http.Client
	repoIdMap    map[string]*gitRepo
}

type gitProject struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

type gitGroup struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type gitUser struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type gitRepository struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Location  string    `json:"location"`
	CreatedAt time.Time `json:"created_at"`
}

type gitTag struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Location string `json:"location"`
}

type gitRepo struct {
	project int
	repo    int
}

func (r *gitlab) gitUrl(pathTemplate string, args ...interface{}) string {
	pathSuffix := fmt.Sprintf(pathTemplate, args...)
	ur := fmt.Sprintf("%s%s%s", r.apiUrl, apiVersion, pathSuffix)
	return ur
}

func newHttpClient(proxy string, insecure bool) (*http.Client, error) {
	hc := &http.Client{
		Timeout: gitTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecure,
			},
		},
	}
	if proxy != "" {
		pxyUrl, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		hc.Transport = &http.Transport{
			Proxy: http.ProxyURL(pxyUrl),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecure,
			},
		}
	}
	return hc, nil
}

func (r *gitlab) newGitlabClient() error {
	var err error
	proxy := GetProxy(r.apiUrl)
	r.gitlabClient, err = newHttpClient(proxy, true)
	return err
}

func (r *gitlab) Login(cfg *share.CLUSRegistryConfig) (error, string) {
	r.apiUrl = cfg.GitlabApiUrl
	r.privateToken = cfg.GitlabPrivateToken

	if err := r.newGitlabClient(); err != nil {
		smd.scanLog.WithFields(log.Fields{"error": err}).Error("")
		return err, err.Error()
	}

	r.newRegClient(cfg.Registry, cfg.Username, cfg.Password)
	r.rc.Alive()

	r.repoIdMap = make(map[string]*gitRepo)
	return nil, ""
}

func (r *gitlab) getData(ur string) ([]byte, error) {
	request, err := http.NewRequest("GET", ur, nil)
	request.Header.Add("PRIVATE-TOKEN", r.privateToken)
	resp, err := r.gitlabClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (r *gitlab) getUsers() ([]gitUser, error) {
	var all []gitUser

	ur := r.gitUrl("/users")
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
	data, err := r.getData(ur)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &all)
	if err != nil {
		return nil, err
	}

	return all, nil
}

func (r *gitlab) getGroups() ([]gitGroup, error) {
	var all []gitGroup

	ur := r.gitUrl("/groups")
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
	data, err := r.getData(ur)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &all)
	if err != nil {
		return nil, err
	}

	return all, nil
}

func (r *gitlab) getProjects() ([]gitProject, error) {
	var all []gitProject
	var projects []gitProject
	var e1, e2, e3 error

	ur := r.gitUrl("/projects")
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
	if data, err := r.getData(ur); err == nil {
		err = json.Unmarshal(data, &all)
	} else {
		e1 = err
	}

	// get user projects
	if users, err := r.getUsers(); err == nil {
		for _, user := range users {
			ur = r.gitUrl(fmt.Sprintf("/users/%d/projects", user.ID))
			smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
			if data, err := r.getData(ur); err == nil {
				if err = json.Unmarshal(data, &projects); err == nil {
					all = append(all, projects...)
				}
			}
		}
	} else {
		e2 = err
	}
	ur = r.gitUrl(fmt.Sprintf("/users/%s/projects", r.username))
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
	if data, err := r.getData(ur); err == nil {
		if err = json.Unmarshal(data, &projects); err == nil {
			all = append(all, projects...)
		}
	} else {
		e2 = err
	}

	// get group projects
	if groups, err := r.getGroups(); err == nil {
		for _, group := range groups {
			ur = r.gitUrl(fmt.Sprintf("/groups/%d/projects?include_subgroups=true", group.ID))
			smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")
			if data, err := r.getData(ur); err == nil {
				if err = json.Unmarshal(data, &projects); err == nil {
					all = append(all, projects...)
				}
			}
		}
	} else {
		e3 = err
	}

	if len(all) == 0 && e1 != nil {
		return nil, e1
	} else if len(all) == 0 && e2 != nil {
		return nil, e2
	} else if len(all) == 0 && e3 != nil {
		return nil, e3
	}

	// remove duplication
	ids := utils.NewSet()
	projects = make([]gitProject, 0)
	for _, p := range all {
		if !ids.Contains(p.ID) {
			ids.Add(p.ID)
			projects = append(projects, p)
		}
	}
	return projects, nil
}

func (r *gitlab) getRepos(id int) ([]gitRepository, error) {
	ur := r.gitUrl("/projects/%d/registry/repositories", id)
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")

	data, err := r.getData(ur)
	if err != nil {
		return nil, err
	}
	var repos []gitRepository
	err = json.Unmarshal(data, &repos)
	if err != nil {
		return nil, err
	}
	return repos, nil
}

func (r *gitlab) getTags(id, repo int) ([]gitTag, error) {
	ur := r.gitUrl("/projects/%d/registry/repositories/%d/tags", id, repo)
	smd.scanLog.WithFields(log.Fields{"url": ur}).Debug("")

	data, err := r.getData(ur)
	if err != nil {
		return nil, err
	}
	var tags []gitTag
	err = json.Unmarshal(data, &tags)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func (r *gitlab) GetRepoList(org, name string, limit int) ([]*share.CLUSImage, error) {
	smd.scanLog.Debug("")

	repos := make([]*share.CLUSImage, 0)
	projects, err := r.getProjects()
	if err != nil {
		return nil, err
	}

	for _, project := range projects {
		rps, err := r.getRepos(project.ID)
		if err != nil {
			smd.scanLog.WithFields(log.Fields{"error": err, "project": project.PathWithNamespace}).Error()
			continue
		}
		for _, rp := range rps {
			repos = append(repos, &share.CLUSImage{Repo: rp.Path})
			r.repoIdMap[rp.Path] = &gitRepo{project: project.ID, repo: rp.ID}
		}
	}

	return repos, nil
}

func (r *gitlab) GetTagList(doamin, repo, tag string) ([]string, error) {
	smd.scanLog.Debug("")

	if rp, ok := r.repoIdMap[repo]; ok {
		if tags, err := r.getTags(rp.project, rp.repo); err == nil {
			ts := make([]string, len(tags))
			for i, tag := range tags {
				ts[i] = tag.Name
			}
			return ts, nil
		} else {
			return nil, err
		}
	}
	return nil, fmt.Errorf("cannot find the repository in map")
}
