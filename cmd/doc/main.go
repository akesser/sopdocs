/*
Copyright 2020 The CRDS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"net/rpc"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crdsdev/doc/pkg/config"
	crdutil "github.com/crdsdev/doc/pkg/crd"
	"github.com/crdsdev/doc/pkg/models"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	flag "github.com/spf13/pflag"
	"github.com/unrolled/render"
	"gopkg.in/robfig/cron.v2"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
)

var db *pgxpool.Pool

// redis connection
var (
	envAnalytics   = "ANALYTICS"
	envDevelopment = "IS_DEV"

	userEnv     = "PG_USER"
	passwordEnv = "PG_PASS"
	hostEnv     = "PG_HOST"
	portEnv     = "PG_PORT"
	dbEnv       = "PG_DB"

	cookieDarkMode = "halfmoon_preferredMode"

	address   string
	analytics bool = false

	gitterChan chan models.GitterRepo

	repoBase   = "github.com"
	configPath = "doc-config.yaml"
	docServer  = DocServer{}
)

// SchemaPlusParent is a JSON schema plus the name of the parent field.
type SchemaPlusParent struct {
	Parent string
	Schema map[string]apiextensions.JSONSchemaProps
}

var page *render.Render

type pageData struct {
	Analytics     bool
	DisableNavBar bool
	IsDarkMode    bool
	Title         string
}

type baseData struct {
	Page pageData
}

type docData struct {
	Page        pageData
	Repo        repoData
	Tag         string
	At          string
	Group       string
	Version     string
	Kind        string
	Description string
	Schema      apiextensions.JSONSchemaProps
}

type orgData struct {
	Page  pageData
	Repo  repoData
	Tag   string
	At    string
	Tags  []string
	CRDs  map[string]models.RepoCRD
	Total int
}

type homeData struct {
	Page           pageData
	Base           string
	ExampleProject string
	Repos          []string
}

type repoData struct {
	Name string `json:"name"`
	Base string `json:"base"`
}

type tagResult struct {
	Start  int                 `json:"start"`
	Limit  int                 `json:"limit"`
	Number int                 `json:"number"`
	Data   map[string][]string `json:"data"`
}

func worker(gitterChan <-chan models.GitterRepo) {
	for job := range gitterChan {
		client, err := rpc.DialHTTP("tcp", "127.0.0.1:1234")
		if err != nil {
			log.Fatal("dialing:", err)
		}
		reply := ""
		if err := client.Call("Gitter.Index", job, &reply); err != nil {
			log.Print("Could not index repo")
		}
	}
}

func tryIndex(repo models.GitterRepo, gitterChan chan models.GitterRepo) bool {
	select {
	case gitterChan <- repo:
		return true
	default:
		return false
	}
}

func init() {
	// TODO(hasheddan): use a flag
	analyticsStr := os.Getenv(envAnalytics)
	if analyticsStr == "true" {
		analytics = true
	}

	gitterChan = make(chan models.GitterRepo, 4)
}

type DocServer struct {
	config *config.DocConfig
}

func main() {
	if repo := os.Getenv("REPO"); repo != "" {
		repoBase = repo
	}
	if path := os.Getenv("CONFIG_FILE"); path != "" {
		configPath = path
	}

	config, err := config.LoadConfigWithDefaults(configPath)

	if err != nil {
		panic(err)
	}

	docServer.config = config

	page = render.New(render.Options{
		Extensions: []string{".html"},
		Directory:  *docServer.config.TemplateFolder,

		Layout:        "layout",
		IsDevelopment: os.Getenv(envDevelopment) == "true",
		Funcs: []template.FuncMap{
			{
				"plusParent": func(p string, s map[string]apiextensions.JSONSchemaProps) *SchemaPlusParent {
					return &SchemaPlusParent{
						Parent: p,
						Schema: s,
					}
				},
			},
		},
	})
	repoBase = os.Getenv("REPO")
	flag.Parse()
	dsn := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", os.Getenv(userEnv), os.Getenv(passwordEnv), os.Getenv(hostEnv), os.Getenv(portEnv), os.Getenv(dbEnv))
	conn, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		panic(err)
	}
	db, err = pgxpool.NewWithConfig(context.Background(), conn)
	if err != nil {
		panic(err)
	}

	for i := 0; i < 4; i++ {
		go worker(gitterChan)
	}

	startCron()
	start()

}

func getPageData(r *http.Request, title string, disableNavBar bool) pageData {
	var isDarkMode = false
	if cookie, err := r.Cookie(cookieDarkMode); err == nil && cookie.Value == "dark-mode" {
		isDarkMode = true
	}
	return pageData{
		Analytics:     analytics,
		IsDarkMode:    isDarkMode,
		DisableNavBar: disableNavBar,
		Title:         title,
	}
}

func start() {
	log.Println("Starting Doc server...")
	r := mux.NewRouter().StrictSlash(true)

	r.Path("/health").Methods(http.MethodGet).HandlerFunc(health)
	r.Path("/api/projects").Methods(http.MethodGet).HandlerFunc(apiTags)
	r.Path("/api/projects").Methods(http.MethodPost, http.MethodOptions).HandlerFunc(indexProject)
	r.Path("/api/projects/search").Methods(http.MethodGet, http.MethodOptions).HandlerFunc(searchProject)

	r.Path("/api/shortcuts/{shortcut}").Methods(http.MethodGet).HandlerFunc(loadShortcut)
	r.Path("/api/shortcuts").Methods(http.MethodPost, http.MethodOptions).HandlerFunc(saveShortcut)

	r.Path("/api/data/{org}/{base}/{repo:.*}@{tag}").Methods(http.MethodGet, http.MethodOptions).HandlerFunc(orgRaw)
	r.Path("/api/data/{org}/{base}/{repo:.*}@{tag}/{tag2}").Methods(http.MethodGet, http.MethodOptions).HandlerFunc(orgRaw)
	r.Path("/api/data/{org}/{base}/{repo:.*}@{tag}/{group}/{kind}/{version}").Methods(http.MethodGet, http.MethodOptions).HandlerFunc(docRaw)
	r.Path("/api/data/{org}/{base}/{repo:.*}@{tag}/{tag2}/{group}/{kind}/{version}").Methods(http.MethodGet, http.MethodOptions).HandlerFunc(docRaw)
	r.Use(mux.CORSMethodMiddleware(r))

	log.Fatal(http.ListenAndServe(":5050", r))
}
func health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// func home(w http.ResponseWriter, r *http.Request) {
// 	data := homeData{Page: getPageData(r, "Doc", true), Base: repoBase, ExampleProject: *docServer.config.ExampleProject}
// 	if err := page.HTML(w, http.StatusOK, "home", data); err != nil {
// 		log.Printf("homeTemplate.Execute(): %v", err)
// 		fmt.Fprint(w, "Unable to render home template.")
// 		return
// 	}
// 	log.Print("successfully rendered home page")
// }

// func raw(w http.ResponseWriter, r *http.Request) {
// 	parameters := mux.Vars(r)
// 	base := parameters["base"]
// 	repo := parameters["repo"]
// 	tag := parameters["tag"]

// 	fullRepo := fmt.Sprintf("%s/%s/%s", repoBase, base, repo)
// 	var rows pgx.Rows
// 	var err error
// 	if tag == "" {
// 		rows, err = db.Query(context.Background(), "SELECT c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.id = (SELECT id FROM tags WHERE LOWER(repo) = LOWER($1) ORDER BY time DESC LIMIT 1);", fullRepo)
// 	} else {
// 		rows, err = db.Query(context.Background(), "SELECT c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.name=$2;", fullRepo, tag)
// 	}

// 	var res []byte
// 	var total []byte
// 	defer rows.Close()
// 	for err == nil && rows.Next() {
// 		if err := rows.Scan(&res); err != nil {
// 			break
// 		}
// 		crd := &apiextensions.CustomResourceDefinition{}
// 		if err := yaml.Unmarshal(res, crd); err != nil {
// 			break
// 		}
// 		crdv1 := &v1.CustomResourceDefinition{}
// 		if err := v1.Convert_apiextensions_CustomResourceDefinition_To_v1_CustomResourceDefinition(crd, crdv1, nil); err != nil {
// 			break
// 		}
// 		crdv1.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
// 		y, err := yaml.Marshal(crdv1)
// 		if err != nil {
// 			break
// 		}
// 		total = append(total, y...)
// 		total = append(total, []byte("\n---\n")...)
// 	}

// 	if err != nil {
// 		fmt.Fprint(w, "Unable to render raw CRDs.")
// 		log.Printf("failed to get raw CRDs for %s : %v", repo, err)
// 	} else {
// 		w.Write([]byte(total))
// 		log.Printf("successfully rendered raw CRDs")
// 	}

// 	if analytics {
// 		u := uuid.New().String()
// 		// TODO(hasheddan): do not hardcode tid and dh
// 		metrics := url.Values{
// 			"v":   {"1"},
// 			"t":   {"pageview"},
// 			"tid": {"UA-116820283-2"},
// 			"cid": {u},
// 			"dh":  {"doc.crds.dev"},
// 			"dp":  {r.URL.Path},
// 			"uip": {r.RemoteAddr},
// 		}
// 		client := &http.Client{}

// 		req, _ := http.NewRequest("POST", "http://www.google-analytics.com/collect", strings.NewReader(metrics.Encode()))
// 		req.Header.Add("User-Agent", r.UserAgent())
// 		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

// 		if _, err := client.Do(req); err != nil {
// 			log.Printf("failed to report analytics: %s", err.Error())
// 		} else {
// 			log.Printf("successfully reported analytics")
// 		}
// 	}
// }

// func org(w http.ResponseWriter, r *http.Request) {
// 	parameters := mux.Vars(r)
// 	base := parameters["base"]
// 	repo := parameters["repo"]
// 	tag := parameters["tag"]
// 	pageData := getPageData(r, fmt.Sprintf("%s/%s", base, repo), false)
// 	fullRepo := fmt.Sprintf("%s/%s/%s", repoBase, base, repo)
// 	b := &pgx.Batch{}
// 	if tag == "" {
// 		b.Queue("SELECT t.name, c.group, c.version, c.kind FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.id = (SELECT id FROM tags WHERE LOWER(repo) = LOWER($1) ORDER BY time DESC LIMIT 1);", fullRepo)
// 	} else {
// 		pageData.Title += fmt.Sprintf("@%s", tag)
// 		b.Queue("SELECT t.name, c.group, c.version, c.kind FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.name=$2;", fullRepo, tag)
// 	}
// 	b.Queue("SELECT name FROM tags WHERE LOWER(repo)=LOWER($1) ORDER BY time DESC;", fullRepo)
// 	br := db.SendBatch(context.Background(), b)
// 	defer br.Close()
// 	c, err := br.Query()
// 	if err != nil {
// 		log.Printf("failed to get CRDs for %s : %v", repo, err)
// 		if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 		return
// 	}
// 	repoCRDs := map[string]models.RepoCRD{}
// 	foundTag := tag
// 	for c.Next() {
// 		var t, g, v, k string
// 		if err := c.Scan(&t, &g, &v, &k); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 		foundTag = t
// 		repoCRDs[g+"/"+v+"/"+k] = models.RepoCRD{
// 			Group:   g,
// 			Version: v,
// 			Kind:    k,
// 		}
// 	}
// 	c, err = br.Query()
// 	if err != nil {
// 		log.Printf("failed to get tags for %s : %v", repo, err)
// 		data := homeData{Page: getPageData(r, "Doc", true), Base: repoBase, ExampleProject: *docServer.config.ExampleProject}
// 		if err := page.HTML(w, http.StatusOK, "home", data); err != nil {
// 			// if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 		return
// 	}
// 	tags := []string{}
// 	tagExists := false
// 	for c.Next() {
// 		var t string
// 		if err := c.Scan(&t); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 		if !tagExists && t == tag {
// 			tagExists = true
// 		}
// 		tags = append(tags, t)
// 	}
// 	if len(tags) == 0 || (!tagExists && tag != "") {
// 		tryIndex(models.GitterRepo{
// 			Org:  base,
// 			Repo: repo,
// 			Tag:  tag,
// 		}, gitterChan)
// 		if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 		return
// 	}
// 	if foundTag == "" {
// 		foundTag = tags[0]
// 	}
// 	if err := page.HTML(w, http.StatusOK, "org", orgData{
// 		Page: pageData,
// 		Repo: repoData{
// 			Name: strings.Join([]string{base, repo}, "/"),
// 			Base: repoBase,
// 		},
// 		Tag:   foundTag,
// 		Tags:  tags,
// 		CRDs:  repoCRDs,
// 		Total: len(repoCRDs),
// 	}); err != nil {
// 		log.Printf("orgTemplate.Execute(): %v", err)
// 		fmt.Fprint(w, "Unable to render org template.")
// 		return
// 	}
// 	log.Printf("successfully rendered org template")
// }

type rawOrgData struct {
	Repo  repoData                  `json:"repo"`
	Tag   string                    `json:"tag"`
	At    string                    `json:"at"`
	Tags  []string                  `json:"tags"`
	CRDs  map[string]models.RepoCRD `json:"crds"`
	Total int                       `json:"total"`
}

func orgRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	parameters := mux.Vars(r)
	org := parameters["org"]
	base := parameters["base"]
	repo := parameters["repo"]
	tag := parameters["tag"]
	tag2 := parameters["tag2"]
	if tag2 != "" {
		tag = tag + "/" + tag2
	}
	pageData := getPageData(r, fmt.Sprintf("%s/%s", org, repo), false)
	fullRepo := fmt.Sprintf("%s/%s/%s", org, base, repo)
	b := &pgx.Batch{}
	if tag == "" {
		b.Queue("SELECT t.name, c.group, c.version, c.kind FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.id = (SELECT id FROM tags WHERE LOWER(repo) = LOWER($1) ORDER BY time DESC LIMIT 1);", fullRepo)
	} else {
		pageData.Title += fmt.Sprintf("@%s", tag)
		b.Queue("SELECT t.name, c.group, c.version, c.kind FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.name=$2;", fullRepo, tag)
	}
	b.Queue("SELECT name FROM tags WHERE LOWER(repo)=LOWER($1) ORDER BY time DESC;", fullRepo)
	br := db.SendBatch(context.Background(), b)
	defer br.Close()
	c, err := br.Query()
	if err != nil {
		log.Printf("failed to get CRDs for %s : %v", repo, err)
		if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
		return
	}
	repoCRDs := map[string]models.RepoCRD{}
	foundTag := tag
	for c.Next() {
		var t, g, v, k string
		if err := c.Scan(&t, &g, &v, &k); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
		foundTag = t
		repoCRDs[g+"/"+v+"/"+k] = models.RepoCRD{
			Group:   g,
			Version: v,
			Kind:    k,
		}
	}
	c, err = br.Query()
	if err != nil {
		log.Printf("failed to get tags for %s : %v", repo, err)
		data := homeData{Page: getPageData(r, "Doc", true), Base: repoBase, ExampleProject: *docServer.config.ExampleProject}
		if err := page.HTML(w, http.StatusOK, "home", data); err != nil {
			// if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
		return
	}
	tags := []string{}
	tagExists := false
	for c.Next() {
		var t string
		if err := c.Scan(&t); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
		if !tagExists && t == tag {
			tagExists = true
		}
		tags = append(tags, t)
	}
	if len(tags) == 0 || (!tagExists && tag != "") {
		tryIndex(models.GitterRepo{
			Org:  org,
			Base: base,
			Repo: repo,
			Tag:  tag,
		}, gitterChan)
		if err := page.HTML(w, http.StatusOK, "new", baseData{Page: pageData}); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
		return
	}
	if foundTag == "" {
		foundTag = tags[0]
	}
	data := rawOrgData{
		Repo: repoData{
			Name: strings.Join([]string{org, repo}, "/"),
			Base: repoBase,
		},
		Tag:   foundTag,
		Tags:  tags,
		CRDs:  repoCRDs,
		Total: len(repoCRDs),
	}
	json.NewEncoder(w).Encode(data)
	log.Printf("successfully rendered org template")
}

// func doc(w http.ResponseWriter, r *http.Request) {
// 	var schema *apiextensions.CustomResourceValidation
// 	crd := &apiextensions.CustomResourceDefinition{}
// 	log.Printf("Request Received: %s\n", r.URL.Path)

// 	parameters := mux.Vars(r)
// 	base := parameters["base"]
// 	repo := parameters["repo"]
// 	tag := parameters["tag"]
// 	kind := parameters["kind"]
// 	group := parameters["group"]
// 	version := parameters["version"]

// 	pageData := getPageData(r, fmt.Sprintf("%s.%s/%s", kind, group, version), false)
// 	fullRepo := fmt.Sprintf("%s/%s/%s", repoBase, base, repo)
// 	var c pgx.Row
// 	if tag == "" {
// 		c = db.QueryRow(context.Background(), "SELECT t.name, c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.id = (SELECT id FROM tags WHERE repo = $1 ORDER BY time DESC LIMIT 1) AND c.group=$2 AND c.version=$3 AND c.kind=$4;", fullRepo, group, version, kind)
// 	} else {
// 		c = db.QueryRow(context.Background(), "SELECT t.name, c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.name=$2 AND c.group=$3 AND c.version=$4 AND c.kind=$5;", fullRepo, tag, group, version, kind)
// 	}
// 	foundTag := tag
// 	if err := c.Scan(&foundTag, crd); err != nil {
// 		log.Printf("failed to get CRDs for %s : %v", repo, err)
// 		if err := page.HTML(w, http.StatusOK, "doc", baseData{Page: pageData}); err != nil {
// 			log.Printf("newTemplate.Execute(): %v", err)
// 			fmt.Fprint(w, "Unable to render new template.")
// 		}
// 	}
// 	schema = crd.Spec.Validation
// 	if len(crd.Spec.Versions) > 1 {
// 		for _, version := range crd.Spec.Versions {
// 			if version.Storage == true {
// 				if version.Schema != nil {
// 					schema = version.Schema
// 				}
// 				break
// 			}
// 		}
// 	}

// 	if schema == nil || schema.OpenAPIV3Schema == nil {
// 		log.Print("CRD schema is nil.")
// 		fmt.Fprint(w, "Supplied CRD has no schema.")
// 		return
// 	}

// 	gvk := crdutil.GetStoredGVK(crd)
// 	if gvk == nil {
// 		log.Print("CRD GVK is nil.")
// 		fmt.Fprint(w, "Supplied CRD has no GVK.")
// 		return
// 	}

// 	if err := page.HTML(w, http.StatusOK, "doc", docData{
// 		Page: pageData,
// 		Repo: repoData{
// 			Name: strings.Join([]string{base, repo}, "/"),
// 			Base: repoBase,
// 		},
// 		Tag:         foundTag,
// 		Group:       gvk.Group,
// 		Version:     gvk.Version,
// 		Kind:        gvk.Kind,
// 		Description: string(schema.OpenAPIV3Schema.Description),
// 		Schema:      *schema.OpenAPIV3Schema,
// 	}); err != nil {
// 		log.Printf("docTemplate.Execute(): %v", err)
// 		fmt.Fprint(w, "Supplied CRD has no schema.")
// 		return
// 	}
// 	log.Printf("successfully rendered doc template")
// }

type rawDocData struct {
	Repo        repoData                      `json:"repo"`
	Tag         string                        `json:"tag"`
	Group       string                        `json:"group"`
	Version     string                        `json:"version"`
	Kind        string                        `json:"kind"`
	Description string                        `json:"description"`
	Schema      apiextensions.JSONSchemaProps `json:"schema"`
}

func docRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var schema *apiextensions.CustomResourceValidation
	crd := &apiextensions.CustomResourceDefinition{}
	log.Printf("Request Received: %s\n", r.URL.Path)

	parameters := mux.Vars(r)
	org := parameters["org"]
	base := parameters["base"]
	repo := parameters["repo"]
	tag := parameters["tag"]
	tag2 := parameters["tag2"]
	kind := parameters["kind"]
	group := parameters["group"]
	version := parameters["version"]

	if tag2 != "" {
		tag = tag + "/" + tag2
	}

	pageData := getPageData(r, fmt.Sprintf("%s.%s/%s", kind, group, version), false)
	fullRepo := fmt.Sprintf("%s/%s/%s", org, base, repo)
	var c pgx.Row
	if tag == "" {
		c = db.QueryRow(context.Background(), "SELECT t.name, c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.id = (SELECT id FROM tags WHERE repo = $1 ORDER BY time DESC LIMIT 1) AND c.group=$2 AND c.version=$3 AND c.kind=$4;", fullRepo, group, version, kind)
	} else {
		c = db.QueryRow(context.Background(), "SELECT t.name, c.data::jsonb FROM tags t INNER JOIN crds c ON (c.tag_id = t.id) WHERE LOWER(t.repo)=LOWER($1) AND t.name=$2 AND c.group=$3 AND c.version=$4 AND c.kind=$5;", fullRepo, tag, group, version, kind)
	}
	foundTag := tag
	if err := c.Scan(&foundTag, crd); err != nil {
		log.Printf("failed to get CRDs for %s : %v", repo, err)
		if err := page.HTML(w, http.StatusOK, "doc", baseData{Page: pageData}); err != nil {
			log.Printf("newTemplate.Execute(): %v", err)
			fmt.Fprint(w, "Unable to render new template.")
		}
	}
	schema = crd.Spec.Validation
	if len(crd.Spec.Versions) > 1 {
		for _, version := range crd.Spec.Versions {
			if version.Storage {
				if version.Schema != nil {
					schema = version.Schema
				}
				break
			}
		}
	}

	if schema == nil || schema.OpenAPIV3Schema == nil {
		log.Print("CRD schema is nil.")
		fmt.Fprint(w, "Supplied CRD has no schema.")
		return
	}

	gvk := crdutil.GetStoredGVK(crd)
	if gvk == nil {
		log.Print("CRD GVK is nil.")
		fmt.Fprint(w, "Supplied CRD has no GVK.")
		return
	}

	data := rawDocData{
		Repo: repoData{
			Name: strings.Join([]string{org, repo}, "/"),
			Base: repoBase,
		},
		Tag:         foundTag,
		Group:       gvk.Group,
		Version:     gvk.Version,
		Kind:        gvk.Kind,
		Description: string(schema.OpenAPIV3Schema.Description),
		Schema:      *schema.OpenAPIV3Schema,
	}
	json.NewEncoder(w).Encode(data)
	log.Printf("successfully rendered doc template")
}

// TODO(hasheddan): add testing and more reliable parse
func parseGHURL(uPath string) (org, repo, group, version, kind, tag string, err error) {
	u, err := url.Parse(uPath)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	elements := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(elements) < 6 {
		return "", "", "", "", "", "", errors.New("invalid path")
	}

	tagSplit := strings.Split(u.Path, "@")
	if len(tagSplit) > 1 {
		tag = tagSplit[1]
	}

	return elements[1], elements[2], elements[3], elements[4], strings.Split(elements[5], "@")[0], tag, nil
}

func searchProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	search := r.FormValue("search")
	start := intFromStringWithDefault(r.FormValue("start"), 0)
	limit := intFromStringWithDefault(r.FormValue("limit"), 100)

	search = "%" + search + "%"

	numberRow := db.QueryRow(context.Background(), "SELECT COUNT(DISTINCT repo) FROM tags WHERE repo LIKE $1", search)

	number := 0
	if err := numberRow.Scan(&number); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(context.Background(), "SELECT u.repo, u.name FROM tags u  WHERE u.repo IN (SELECT DISTINCT t.repo FROM tags t WHERE t.repo LIKE $1  LIMIT $2 OFFSET $3) ORDER BY u.repo, u.time DESC", search, limit, start)

	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	data := map[string][]string{}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			log.Fatal("error while iterating dataset")
		}

		// convert DB types to Go types
		repo := values[0].(string)
		tag := values[1].(string)

		data[repo] = append(data[repo], tag)

		//repo := values[1].(string)

	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	result := tagResult{
		Number: number,
		Start:  start,
		Limit:  limit,
		Data:   data,
	}

	json.NewEncoder(w).Encode(result)
	// io.WriteString(w, fmt.Sprintf(`{"start": %d, "limit": %d}`, start, limit))
}
func apiTags(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	start := intFromStringWithDefault(r.FormValue("start"), 0)
	limit := intFromStringWithDefault(r.FormValue("limit"), 100)

	fmt.Println(db.Stat().TotalConns())
	fmt.Println(db.Stat().IdleConns())
	fmt.Println(db.Stat().MaxConns())
	numberRow := db.QueryRow(context.Background(), "SELECT COUNT(DISTINCT repo) FROM tags ")

	number := 0
	if err := numberRow.Scan(&number); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(context.Background(), "SELECT u.repo, u.name FROM tags u  WHERE u.repo IN (SELECT DISTINCT t.repo FROM tags t ORDER BY t.repo LIMIT $1 OFFSET $2) ORDER BY u.repo, u.time DESC", limit, start)

	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	data := map[string][]string{}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			log.Fatal("error while iterating dataset")
		}

		// convert DB types to Go types
		repo := values[0].(string)
		tag := values[1].(string)

		data[repo] = append(data[repo], tag)
		//repo := values[1].(string)

	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	result := tagResult{
		Number: number,
		Start:  start,
		Limit:  limit,
		Data:   data,
	}

	json.NewEncoder(w).Encode(result)
	// io.WriteString(w, fmt.Sprintf(`{"start": %d, "limit": %d}`, start, limit))
}

func intFromStringWithDefault(input string, defaultValue int) int {
	if l, err := strconv.Atoi(input); err == nil {
		return l
	}
	return defaultValue
}

type Repo struct {
	Org  string `json:"org"`
	Base string `json:"base"`
	Repo string `json:"repo"`
}

type IndexResult struct {
	Error *string `json:"error"`
}

func indexProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Content-Type", "application/json")

	var repo Repo
	if err := json.NewDecoder(r.Body).Decode(&repo); err != nil {
		errorString := "error decoding body"
		result := IndexResult{
			Error: &errorString,
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	gitterRepo := models.GitterRepo{
		Tag:  "",
		Org:  repo.Org, // TODO: Update the logic of org and base
		Repo: repo.Repo,
		Base: repo.Base,
	}
	client, err := rpc.DialHTTP("tcp", "127.0.0.1:1234")
	if err != nil {
		log.Fatal("dialing:", err)
	}
	reply := ""
	if err := client.Call("Gitter.Index", gitterRepo, &reply); err != nil {
		log.Print("Could not index repo")
		errorString := fmt.Sprintf("could not find %s/%s", repo.Org, repo.Repo)
		result := IndexResult{
			Error: &errorString,
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	repoString := fmt.Sprintf("%s/%s/%s", repoBase, repo.Base, repo.Repo)
	result, err := findTagsForProject(repoString)
	if err != nil {
		errorString := fmt.Sprintf("could not load data for %s/%s", repo.Org, repo.Repo)
		result := IndexResult{
			Error: &errorString,
		}
		json.NewEncoder(w).Encode(result)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

func findTagsForProject(repo string) (*tagResult, error) {

	numberRow := db.QueryRow(context.Background(), "SELECT COUNT(DISTINCT repo) FROM tags ")

	number := 0
	if err := numberRow.Scan(&number); err != nil {
		log.Fatal(err)
		return nil, err
	}

	rows, err := db.Query(context.Background(), "SELECT u.repo, u.name FROM tags u  WHERE u.repo IN (SELECT DISTINCT t.repo FROM tags t WHERE t.repo = $1) ORDER BY u.repo, u.time DESC", repo)
	if err != nil {
		log.Fatal(err)
		return nil, err
	}

	data := map[string][]string{}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			log.Fatal("error while iterating dataset")
			return nil, err
		}

		// convert DB types to Go types
		repo := values[0].(string)
		tag := values[1].(string)

		data[repo] = append(data[repo], tag)

	}

	result := tagResult{
		Number: number,
		Start:  1,
		Limit:  1,
		Data:   data,
	}

	return &result, nil
}

func updateProject() {

	limit := 2
	// interval := 1
	rows, err := db.Query(context.Background(), "SELECT repo FROM (SELECT DISTINCT ON (repo) repo FROM tags  WHERE repo NOT IN (SELECT repo FROM lastupdates WHERE lastupdate > (NOW() - INTERVAL '1' HOUR))) AS u ORDER BY RANDOM() LIMIT $1", limit)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	repos := []*models.GitterRepo{}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			log.Fatal("error while iterating dataset")
		}

		// convert DB types to Go types
		repo := values[0].(string)
		gitterRepo := getRepoFromString(repo)
		if gitterRepo != nil {
			repos = append(repos, gitterRepo)
		}

	}

	if len(repos) > 0 {

		reply := ""
		if err == nil {
			for i := range repos {
				repo := *repos[i]
				client, err := rpc.DialHTTP("tcp", "127.0.0.1:1234")
				if err != nil {
					log.Println("dialing:", err)
				}
				err = client.Call("Gitter.Index", repo, &reply)

				if err != nil {
					log.Println("Error while updating entry ", err)
				} else {
					updateEntry(repo)
				}
			}
		}
	}
}

func updateEntry(repo models.GitterRepo) {

	repoString := getStringFromRepo(repo)
	result, err := db.Query(context.Background(), "INSERT INTO lastupdates (repo, lastupdate) VALUES ($1, NOW()) ON CONFLICT (repo) DO UPDATE SET lastupdate= NOW()", repoString)

	if err != nil {

	}
	defer result.Close()
	if result != nil {

	}
}

func getRepoFromString(input string) *models.GitterRepo {

	parts := strings.Split(input, "/")

	if len(parts) > 2 {
		org := parts[0]
		base := strings.Join(parts[1:len(parts)-1], "/")
		repo := parts[len(parts)-1]

		gitterRepo := models.GitterRepo{
			Tag:  "",
			Org:  org,
			Base: base, // TODO: Update the logic of org and base
			Repo: repo,
		}
		return &gitterRepo
	}
	return nil
}

func getStringFromRepo(input models.GitterRepo) string {
	result := input.Org + "/" + input.Base + "/" + input.Repo
	return result
}

func startCron() {
	c := cron.New()
	c.AddFunc("@every 1m", func() { updateProject() })
	c.AddFunc("@every 5m", func() { deleteInvalidShortcuts() })
	c.Start()
}

type ShortcutError struct {
	Error string `json:"error"`
}

func loadShortcut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		return
	}

	parameters := mux.Vars(r)
	shortcut := parameters["shortcut"]

	dataRow := db.QueryRow(context.Background(), "SELECT org, base, repo, tag, \"group\", kind, version, yaml_data FROM shortcuts WHERE shortcut = $1 AND valid_until > Now()", shortcut)

	var org, base, repo, tag, group, kind, version, yamlData string
	if err := dataRow.Scan(&org, &base, &repo, &tag, &group, &kind, &version, &yamlData); err != nil {
		e := ShortcutError{
			Error: "could not find shortcut",
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(e)
		return
	}

	result := Shortcut{
		Org:      org,
		Base:     base,
		Repo:     repo,
		Tag:      tag,
		Group:    group,
		Kind:     kind,
		Version:  version,
		YamlData: yamlData,
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

type Shortcut struct {
	Org      string `json:"org"`
	Base     string `json:"base"`
	Repo     string `json:"repo"`
	Tag      string `json:"tag"`
	Group    string `json:"group"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	YamlData string `json:"yamlData"`
}
type DBShortcut struct {
	Org      string `json:"org"`
	Base     string `json:"base"`
	Repo     string `json:"repo"`
	Tag      string `json:"tag"`
	Group    string `json:"group"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	YamlData string `json:"yamlData"`
}

type ShortcutString struct {
	Shortcut   string    `json:"shortcut"`
	ValidUntil time.Time `json:"validUntil"`
}

func saveShortcut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodOptions {
		return
	}
	var shortcut Shortcut

	if err := json.NewDecoder(r.Body).Decode(&shortcut); err != nil {
		errorString := "error decoding body"
		result := IndexResult{
			Error: &errorString,
		}
		json.NewEncoder(w).Encode(result)
		return
	}

	stringForHash := fmt.Sprintf("%s::%s::%s::%s::%s::%s::%s::%s", shortcut.Org, shortcut.Base, shortcut.Repo, shortcut.Tag, shortcut.Group, shortcut.Kind, shortcut.Version, shortcut.YamlData)

	hash := fmt.Sprintf("%x", md5.Sum([]byte(stringForHash)))

	hashRow := db.QueryRow(context.Background(), "SELECT shortcut FROM shortcuts WHERE hash = $1", hash)

	var shortcutString string
	err := hashRow.Scan(&shortcutString)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		errorString := "error loading data"
		result := IndexResult{
			Error: &errorString,
		}
		json.NewEncoder(w).Encode(result)
		return
	}

	validUntil := time.Now().Add(2 * 7 * 24 * time.Hour)

	if shortcutString != "" {
		db.Exec(context.Background(), "UPDATE shortcuts SET valid_until = $1", validUntil)

	} else {
		shortcutString = findShortcut()
		_, err := db.Exec(context.Background(), "INSERT INTO shortcuts (shortcut, org, base, repo,tag, \"group\", kind, version, yaml_data, valid_until, hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", shortcutString, shortcut.Org, shortcut.Base, shortcut.Repo, shortcut.Tag, shortcut.Group, shortcut.Kind, shortcut.Version, shortcut.YamlData, validUntil, hash)
		if err != nil {

		}

	}

	result := ShortcutString{
		Shortcut:   shortcutString,
		ValidUntil: validUntil,
	}
	json.NewEncoder(w).Encode(result)
}

func deleteInvalidShortcuts() {
	// delete shortcut entries that are already invalid
	db.Exec(context.Background(), "DELETE FROM shortcuts WHERE valid_until < NOW()")
}

func findShortcut() string {
	numTries := 0
	for {
		shortcut := RandomString(20)
		hashRow := db.QueryRow(context.Background(), "SELECT COUNT(shortcut) FROM shortcuts WHERE shortcut = $1", shortcut)

		var number int
		hashRow.Scan(&number)
		if number == 0 {
			return shortcut
		}
		if numTries >= 20 {
			return ""
		}
		numTries++
	}
}
func RandomString(n int) string {
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

	s := make([]rune, n)
	for i := range s {
		s[i] = letters[rand.Intn(len(letters))]
	}
	return string(s)
}
