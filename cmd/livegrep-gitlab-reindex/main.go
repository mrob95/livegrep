package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/xanzy/go-gitlab"

	"github.com/livegrep/livegrep/src/proto/config"
)

const BLDeprecatedMessage = "This flag has been deprecated and will be removed in a future release. Please switch to the '-ignorelist' option."

var (
	flagCodesearch   = flag.String("codesearch", "", "Path to the `codesearch` binary")
	flagFetchReindex = flag.String("fetch-reindex", "", "Path to the `livegrep-fetch-reindex` binary")
	flagApiBaseUrl   = flag.String("api-base-url", "https://gitlab.example.com/api/v4", "Gitlab API base url")
	flagGitlabToken  = flag.String("gitlab-token", os.Getenv("GITLAB_TOKEN"), "Gitlab access token")
	flagRepoDir      = flag.String("dir", "repos", "Directory to store repos")
	flagIgnorelist   = flag.String("ignorelist", "", "File containing a list of repositories to ignore when indexing")
	flagIndexPath    = dynamicDefault{
		display: "${dir}/livegrep.idx",
		fn:      func() string { return path.Join(*flagRepoDir, "livegrep.idx") },
	}
	flagRevision             = flag.String("revision", "HEAD", "git revision to index")
	flagUrlPattern           = flag.String("url-pattern", "https://gitlab.com/{name}/-/blob/{version}/{path}#L{lno}", "when using the local frontend fileviewer, this string will be used to construt a link to the file source on gitlab")
	flagName                 = flag.String("name", "livegrep index", "The name to be stored in the index file")
	flagNumRepoUpdateWorkers = flag.String("num-repo-update-workers", "8", "Number of workers fetch-reindex will use to update repositories")
	flagRevparse             = flag.Bool("revparse", true, "whether to `git rev-parse` the provided revision in generated links")
	flagForks                = flag.Bool("forks", true, "whether to index repositories that are forks, and not original repos")
	flagArchived             = flag.Bool("archived", false, "whether to index repositories that are archived on gitlab")
	flagHTTP                 = flag.Bool("http", false, "clone repositories over HTTPS instead of SSH")
	flagHTTPUsername         = flag.String("http-user", "git", "Override the username to use when cloning over https")
	flagDepth                = flag.Int("depth", 0, "clone repository with specify --depth=N depth.")
	flagSkipMissing          = flag.Bool("skip-missing", false, "skip repositories where the specified revision is missing")
	flagNoIndex              = flag.Bool("no-index", false, "Skip indexing after writing config and fetching")

	// TODO: think about how to implement these or something similar for gitlab,
	// rather than just listing all of the projects that are accessible
	// flagRepos = stringList{}
	flagGroups = stringList{}
	// flagUsers = stringList{}
)

func init() {
	flag.Var(&flagIndexPath, "out", "Path to write the index")
	// flag.Var(&flagRepos, "repo", "Specify a repo to index (may be passed multiple times)")
	flag.Var(&flagGroups, "group", "Specify a gitlab group to index (may be passed multiple times)")
	// flag.Var(&flagUsers, "user", "Specify a github user to index (may be passed multiple times)")
}

const Workers = 8

func main() {
	flag.Parse()
	log.SetFlags(0)

	var ignorelist map[string]struct{}
	if *flagIgnorelist != "" {
		var err error
		ignorelist, err = loadIgnorelist(*flagIgnorelist)
		if err != nil {
			log.Fatalf("loading %s: %s", *flagIgnorelist, err)
		}
	}

	git, err := gitlab.NewClient(*flagGitlabToken, gitlab.WithBaseURL(*flagApiBaseUrl))
	if err != nil {
		log.Fatalf("creating gitlab client: %s", err)
	}

	repos, err := loadRepos(git, flagGroups.strings)
	if err != nil {
		log.Fatalln(err.Error())
	}

	repos = filterRepos(repos, ignorelist, !*flagForks, !*flagArchived)

	sort.Sort(ReposByName(repos))

	config, err := buildConfig(*flagName, *flagRepoDir, repos, *flagRevision)
	if err != nil {
		log.Fatalln(err.Error())
	}
	configPath := path.Join(*flagRepoDir, "livegrep.json")
	if err := writeConfig(config, configPath); err != nil {
		log.Fatalln(err.Error())
	}

	index := flagIndexPath.Get().(string)

	args := []string{
		"--out", index,
		"--codesearch", *flagCodesearch,
		"--num-workers", *flagNumRepoUpdateWorkers,
	}
	if *flagNoIndex {
		args = append(args, "--no-index")
	}
	if *flagRevparse {
		args = append(args, "--revparse")
	}
	if *flagSkipMissing {
		args = append(args, "--skip-missing")
	}
	args = append(args, configPath)

	if *flagFetchReindex == "" {
		fr := findBinary("livegrep-fetch-reindex")
		flagFetchReindex = &fr
	}

	log.Printf("Running: %s %v\n", *flagFetchReindex, args)
	cmd := exec.Command(*flagFetchReindex, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if *flagGitlabToken != "" {
		cmd.Env = append(os.Environ(), fmt.Sprintf("GITLAB_TOKEN=%s", *flagGitlabToken))
	}
	if err := cmd.Run(); err != nil {
		log.Fatalln("livegrep-fetch-reindex: ", err)
	}
}

func findBinary(name string) string {
	paths := []string{
		path.Join(path.Dir(os.Args[0]), name),
		strings.Replace(os.Args[0], path.Base(os.Args[0]), name, -1),
	}
	for _, try := range paths {
		if st, err := os.Stat(try); err == nil && (st.Mode()&os.ModeDir) == 0 {
			return try
		}
	}
	return name
}

type ReposByName []*gitlab.Project

func (r ReposByName) Len() int           { return len(r) }
func (r ReposByName) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r ReposByName) Less(i, j int) bool { return r[i].PathWithNamespace < r[j].PathWithNamespace }

func loadIgnorelist(path string) (map[string]struct{}, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	out := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		out[l] = struct{}{}
	}
	return out, nil
}

func loadRepos(client *gitlab.Client, groups []string) ([]*gitlab.Project, error) {
	var projects []*gitlab.Project

	// TODO: parallelise, deduplicate, etc
	if len(groups) > 0 {
		for _, group := range groups {
			opt := &gitlab.ListGroupProjectsOptions{
				ListOptions: gitlab.ListOptions{
					PerPage: 100,
					Page:    1,
				},
			}
			for {
				ps, resp, err := client.Groups.ListGroupProjects(group, opt)
				if err != nil {
					return nil, err
				}
				projects = append(projects, ps...)
				if resp.NextPage == 0 {
					break
				}
				opt.Page = resp.NextPage
			}
		}
		return projects, nil
	}

	// read everything the user has access to
	opt := &gitlab.ListProjectsOptions{
		Archived: gitlab.Bool(*flagArchived),
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
			Page:    1,
		},
	}
	for {
		ps, resp, err := client.Projects.ListProjects(opt)
		if err != nil {
			return nil, err
		}
		projects = append(projects, ps...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return projects, nil
}

func filterRepos(repos []*gitlab.Project,
	ignorelist map[string]struct{},
	excludeForks bool, excludeArchived bool) []*gitlab.Project {
	var out []*gitlab.Project

	for _, r := range repos {
		if excludeForks && r.ForkedFromProject != nil {
			log.Printf("Excluding fork %s, was forked from %s", r.PathWithNamespace, r.ForkedFromProject.PathWithNamespace)
			continue
		}
		if ignorelist != nil {
			if _, ok := ignorelist[r.PathWithNamespace]; ok {
				continue
			}
		}
		out = append(out, r)
	}

	return out
}

func writeConfig(config []byte, file string) error {
	dir := path.Dir(file)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return ioutil.WriteFile(file, config, 0644)
}

func buildConfig(name string,
	dir string,
	repos []*gitlab.Project,
	revision string) ([]byte, error) {
	cfg := config.IndexSpec{
		Name: name,
	}

	for _, r := range repos {
		if *flagSkipMissing {
			cmd := exec.Command("git",
				"--git-dir",
				path.Join(dir, r.PathWithNamespace),
				"rev-parse",
				"--verify",
				revision,
			)
			if e := cmd.Run(); e != nil {
				log.Printf("Skipping missing revision repo=%s rev=%s",
					r.PathWithNamespace, revision,
				)
				continue
			}
		}
		var remote string
		remote = r.SSHURLToRepo
		if *flagHTTP {
			remote = r.HTTPURLToRepo
		}

		var password_env string
		if *flagGitlabToken != "" {
			password_env = "GITLAB_TOKEN"
		}

		cfg.Repositories = append(cfg.Repositories, &config.RepoSpec{
			Path:      path.Join(dir, r.PathWithNamespace),
			Name:      r.PathWithNamespace,
			Revisions: []string{revision},
			Metadata: &config.Metadata{
				// TODO: what is this used for?
				Github:     r.WebURL,
				Remote:     remote,
				UrlPattern: *flagUrlPattern,
			},
			CloneOptions: &config.CloneOptions{
				Depth:       int32(*flagDepth),
				Username:    *flagHTTPUsername,
				PasswordEnv: password_env,
			},
		})
	}

	return json.MarshalIndent(cfg, "", "  ")
}
