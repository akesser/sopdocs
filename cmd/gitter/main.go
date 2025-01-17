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
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/crdsdev/doc/pkg/config"
	"github.com/crdsdev/doc/pkg/crd"
	"github.com/crdsdev/doc/pkg/models"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
	"gopkg.in/square/go-jose.v2/json"
	yaml "gopkg.in/yaml.v3"
)

const (
	crdArgCount = 6

	userEnv     = "PG_USER"
	passwordEnv = "PG_PASS"
	hostEnv     = "PG_HOST"
	portEnv     = "PG_PORT"
	dbEnv       = "PG_DB"
)

var repoBase = "github.com"

// var token = ""

var tokens = map[string]string{}

type DocGitter struct {
	config *config.DocConfig
}

var (
	configPath = "doc-config.yaml"
	docGitter  = DocGitter{}
)

func main() {
	if path := os.Getenv("CONFIG_FILE"); path != "" {
		configPath = path
	}
	docconfig, err := config.LoadConfigWithDefaults(configPath)
	docGitter.config = docconfig

	tokens = config.LoadTokensFromEnv(docconfig.TokenVariables)
	repoBase = os.Getenv("REPO")
	dsn := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", os.Getenv(userEnv), os.Getenv(passwordEnv), os.Getenv(hostEnv), os.Getenv(portEnv), os.Getenv(dbEnv))
	conn, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		panic(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), conn)
	if err != nil {
		panic(err)
	}
	gitter := &Gitter{
		conn: pool,
	}
	// startCron()
	rpc.Register(gitter)
	rpc.HandleHTTP()
	l, e := net.Listen("tcp", ":1234")
	if e != nil {
		log.Fatal("listen error:", e)
	}
	log.Println("Starting gitter...")
	http.Serve(l, nil)
}

// Gitter indexes git repos.
type Gitter struct {
	conn *pgxpool.Pool
}

type tag struct {
	timestamp time.Time
	hash      plumbing.Hash
	name      string
}

// Index indexes a git repo at the specified url.
func (g *Gitter) Index(gRepo models.GitterRepo, reply *string) error {
	log.Printf("Indexing repo %s/%s...\n", gRepo.Org, gRepo.Repo)

	dir, err := ioutil.TempDir(os.TempDir(), "doc-gitter")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fullRepo := fmt.Sprintf("%s/%s/%s", strings.ToLower(gRepo.Org), strings.ToLower(gRepo.Base), strings.ToLower(gRepo.Repo))
	repoURL := fmt.Sprintf("https://%s", fullRepo)
	token := tokens[strings.ToLower(gRepo.Org)]
	if token != "" {
		repoURL = fmt.Sprintf("https://oauth2:%s@%s", token, fullRepo)
	}
	cloneOpts := &git.CloneOptions{
		URL:               repoURL,
		Depth:             1,
		Progress:          os.Stdout,
		RecurseSubmodules: git.NoRecurseSubmodules,
	}
	if gRepo.Tag != "" {
		cloneOpts.ReferenceName = plumbing.NewTagReferenceName(gRepo.Tag)
		cloneOpts.SingleBranch = true
	}
	repo, err := git.PlainClone(dir, false, cloneOpts)
	if err != nil {
		return err
	}
	iter, err := repo.Tags()
	if err != nil {
		return err
	}
	w, err := repo.Worktree()
	if err != nil {
		return err
	}
	// Get CRDs for each tag
	tags := []tag{}
	if err := iter.ForEach(func(obj *plumbing.Reference) error {
		if gRepo.Tag == "" {
			tags = append(tags, tag{
				hash: obj.Hash(),
				name: obj.Name().Short(),
			})
			return nil
		}
		if obj.Name().Short() == gRepo.Tag {
			tags = append(tags, tag{
				hash: obj.Hash(),
				name: obj.Name().Short(),
			})
			iter.Close()
		}
		return nil
	}); err != nil {
		log.Println(err)
	}
	for _, t := range tags {
		h, err := repo.ResolveRevision(plumbing.Revision(t.hash.String()))
		if err != nil || h == nil {
			log.Printf("Unable to resolve revision: %s (%v)", t.hash.String(), err)
			continue
		}
		c, err := repo.CommitObject(*h)
		if err != nil || c == nil {
			log.Printf("Unable to resolve revision: %s (%v)", t.hash.String(), err)
			continue
		}
		r := g.conn.QueryRow(context.Background(), "SELECT id FROM tags WHERE name=$1 AND repo=$2", t.name, fullRepo)
		var tagID int
		if err := r.Scan(&tagID); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			r := g.conn.QueryRow(context.Background(), "INSERT INTO tags(name, repo, time) VALUES ($1, $2, $3) RETURNING id", t.name, fullRepo, c.Committer.When)
			if err := r.Scan(&tagID); err != nil {
				return err
			}
		}
		repoCRDs, err := getCRDsFromTag(dir, t.name, h, w)
		if err != nil {
			log.Printf("Unable to get CRDs: %s@%s (%v)", repo, t.name, err)
			continue
		}
		if len(repoCRDs) > 0 {
			allArgs := make([]interface{}, 0, len(repoCRDs)*crdArgCount)
			for _, crd := range repoCRDs {
				allArgs = append(allArgs, crd.Group, crd.Version, crd.Kind, tagID, crd.Filename, crd.CRD)
			}
			if _, err := g.conn.Exec(context.Background(), buildInsert("INSERT INTO crds(\"group\", version, kind, tag_id, filename, data) VALUES ", crdArgCount, len(repoCRDs))+"ON CONFLICT DO NOTHING", allArgs...); err != nil {
				return err
			}
		}
	}

	log.Printf("Finished indexing %s/%s\n", gRepo.Org, gRepo.Repo)

	return nil
}

func getCRDsFromTag(dir string, tag string, hash *plumbing.Hash, w *git.Worktree) (map[string]models.RepoCRD, error) {
	err := w.Checkout(&git.CheckoutOptions{
		Hash:  *hash,
		Force: true,
	})
	if err != nil {
		return nil, err
	}
	if err := w.Reset(&git.ResetOptions{
		Mode: git.HardReset,
	}); err != nil {
		return nil, err
	}
	reg := regexp.MustCompile("kind: CustomResourceDefinition")
	regPath := regexp.MustCompile(`^.*\.yaml`)
	g, _ := w.Grep(&git.GrepOptions{
		Patterns:  []*regexp.Regexp{reg},
		PathSpecs: []*regexp.Regexp{regPath},
	})
	repoCRDs := map[string]models.RepoCRD{}
	files := getYAMLs(g, dir)

	ignoreRegexList := []*regexp.Regexp{}
	ignoreFilesList := []*string{}
	ignoreGroupsList := []*string{}
	ignoreVersionsList := []*string{}

	if docGitter.config != nil && docGitter.config.Scan != nil && docGitter.config.Scan.Ignore != nil {
		if docGitter.config.Scan.Ignore.Regexes != nil {
			regesList, err := compileToRegex(docGitter.config.Scan.Ignore.Regexes)
			if err != nil {
				panic(err)
			}
			ignoreRegexList = append(ignoreRegexList, regesList...)
		}
		if docGitter.config.Scan.Ignore.Files != nil {
			ignoreFilesList = append(ignoreFilesList, docGitter.config.Scan.Ignore.Files...)
		}
		if docGitter.config.Scan.Ignore.Groups != nil {
			ignoreGroupsList = append(ignoreGroupsList, docGitter.config.Scan.Ignore.Groups...)
		}
		if docGitter.config.Scan.Ignore.Versions != nil {
			ignoreVersionsList = append(ignoreVersionsList, docGitter.config.Scan.Ignore.Versions...)
		}
	}

	ignorefile, err := getRepoIgnoreFile(dir)

	if err == nil {

		if ignorefile.Regexes != nil {
			regesList, err := compileToRegex(ignorefile.Regexes)
			if err != nil {
				panic(err)
			}
			ignoreRegexList = append(ignoreRegexList, regesList...)
		}
		if ignorefile.Files != nil {
			ignoreFilesList = append(ignoreFilesList, ignorefile.Files...)
		}
		if ignorefile.Groups != nil {
			ignoreGroupsList = append(ignoreGroupsList, ignorefile.Groups...)
		}
		if ignorefile.Versions != nil {
			ignoreVersionsList = append(ignoreVersionsList, ignorefile.Versions...)
		}
	}
	for file, yamls := range files {
		if isInList(file, ignoreFilesList) {
			continue
		}
		if isMatchInList(file, ignoreRegexList) {
			continue
		}
		for _, y := range yamls {
			crder, err := crd.NewCRDer(y, crd.StripLabels(), crd.StripAnnotations(), crd.StripConversion())
			if err != nil || crder.CRD == nil {
				continue
			}
			cbytes, err := json.Marshal(crder.CRD)
			if err != nil {
				continue
			}

			if isInList(crder.GVK.Group, ignoreGroupsList) {
				continue
			}

			if isInList(crder.GVK.Version, ignoreVersionsList) {
				continue
			}
			repoCRDs[crd.PrettyGVK(crder.GVK)] = models.RepoCRD{
				Path:     crd.PrettyGVK(crder.GVK),
				Filename: path.Base(file),
				Group:    crder.GVK.Group,
				Version:  crder.GVK.Version,
				Kind:     crder.GVK.Kind,
				CRD:      cbytes,
			}
		}
	}
	return repoCRDs, nil
}

func compileToRegex(list []*string) (regexList []*regexp.Regexp, err error) {

	for _, reg := range list {
		regex, err := regexp.Compile(*reg)
		if err != nil {
			return nil, err
		}
		regexList = append(regexList, regex)

	}

	return regexList, nil
}

func isInList(a string, list []*string) bool {
	if len(list) > 0 {
		for _, entry := range list {
			if a == *entry {
				return true
			}
		}
	}
	return false
}

func isMatchInList(a string, list []*regexp.Regexp) bool {
	if len(list) > 0 {
		for _, entry := range list {
			if entry.MatchString(a) {
				return true
			}
		}
	}
	return false
}

func getRepoIgnoreFile(dir string) (*config.IgnoreConfig, error) {
	filename := docGitter.config.RepositoryConfigFile
	content, err := os.ReadFile(dir + "/" + *filename)

	if err == nil {
		var repoConfig config.RepoConfig

		err = yaml.Unmarshal(content, &repoConfig)

		if err != nil {
			return nil, err
		}
		if repoConfig.Ignore != nil {
			return repoConfig.Ignore, nil
		}
	}
	return nil, errors.New("found no ignorefile")

}

func getYAMLs(greps []git.GrepResult, dir string) map[string][][]byte {
	allCRDs := map[string][][]byte{}
	for _, res := range greps {
		b, err := ioutil.ReadFile(dir + "/" + res.FileName)
		if err != nil {
			log.Printf("failed to read CRD file: %s", res.FileName)
			continue
		}

		yamls, err := splitYAML(b, res.FileName)
		if err != nil {
			log.Printf("failed to split/parse CRD file: %s", res.FileName)
			continue
		}

		allCRDs[res.FileName] = yamls
	}
	return allCRDs
}

func splitYAML(file []byte, filename string) ([][]byte, error) {
	var yamls [][]byte
	var err error = nil
	defer func() {
		if err := recover(); err != nil {
			yamls = make([][]byte, 0)
			err = fmt.Errorf("panic while processing yaml file: %w", err)
		}
	}()

	decoder := yaml.NewDecoder(bytes.NewReader(file))
	for {
		var node map[string]interface{}
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("failed to decode part of CRD file: %s\n%s", filename, err)
			continue
		}

		doc, err := yaml.Marshal(node)
		if err != nil {
			log.Printf("failed to encode part of CRD file: %s\n%s", filename, err)
			continue
		}
		yamls = append(yamls, doc)
	}
	return yamls, err
}

func buildInsert(query string, argsPerInsert, numInsert int) string {
	absArg := 1
	for i := 0; i < numInsert; i++ {
		query += "("
		for j := 0; j < argsPerInsert; j++ {
			query += "$" + fmt.Sprint(absArg)
			if j != argsPerInsert-1 {
				query += ","
			}
			absArg++
		}
		query += ")"
		if i != numInsert-1 {
			query += ","
		}
	}
	return query
}

// var db *pgxpool.Pool

// func getRepoFromString(input string) *models.GitterRepo {

// 	parts := strings.Split(input, "/")

// 	if len(parts) > 2 {
// 		org := parts[0]
// 		base := strings.Join(parts[1:len(parts)-1], "/")
// 		repo := parts[len(parts)-1]

// 		gitterRepo := models.GitterRepo{
// 			Tag:  "",
// 			Org:  org,
// 			Base: base, // TODO: Update the logic of org and base
// 			Repo: repo,
// 		}
// 		return &gitterRepo
// 	}
// 	return nil
// }

// func getStringFromRepo(input models.GitterRepo) string {
// 	result := input.Org + "/" + input.Base + "/" + input.Repo
// 	return result
// }

// func updateProject() {

// 	limit := 2
// 	// interval := 1
// 	rows, err := db.Query(context.Background(), "SELECT repo FROM (SELECT DISTINCT ON (repo) repo FROM tags  WHERE repo NOT IN (SELECT repo FROM lastupdates WHERE lastupdate > (NOW() - INTERVAL '1' HOUR))) AS u ORDER BY RANDOM() LIMIT $1", limit)
// 	if err != nil {
// 		log.Fatal(err)
// 	}

// 	repos := []*models.GitterRepo{}

// 	for rows.Next() {
// 		values, err := rows.Values()
// 		if err != nil {
// 			log.Fatal("error while iterating dataset")
// 		}

// 		// convert DB types to Go types
// 		repo := values[0].(string)
// 		gitterRepo := getRepoFromString(repo)
// 		if gitterRepo != nil {
// 			repos = append(repos, gitterRepo)
// 		}

// 	}

// 	if len(repos) > 0 {

// 		reply := ""
// 		if err == nil {
// 			for i := range repos {
// 				repo := *repos[i]
// 				err = gitter.Index(repo, &reply)
// 				if err != nil {
// 					log.Fatal("Error while updating entry", err)
// 				} else {
// 					updateEntry(repo)
// 				}
// 			}
// 		}
// 	}
// }

// func updateEntry(repo models.GitterRepo) {

// 	repoString := getStringFromRepo(repo)
// 	result, err := db.Query(context.Background(), "INSERT INTO lastupdates (repo, lastupdate) VALUES ($1, NOW()) ON CONFLICT (repo) DO UPDATE SET lastupdate= NOW()", repoString)
// 	if err != nil {

// 	}
// 	if result != nil {

// 	}
// }

// func startCron() {
// 	c := cron.New()
// 	c.AddFunc("@every 1m", func() { updateProject() })
// 	c.Start()
// }
