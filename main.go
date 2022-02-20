package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/containers/image/v5/docker/reference"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad/api"
	"github.com/olekukonko/tablewriter"
	"golang.org/x/sync/errgroup"
)

type Instance struct {
	Namespace string
	Job       string
	Group     string
	Task      string
	Image     reference.NamedTagged
}

type WatchedImage struct {
	Name    string       `toml:"name"`
	Include []TOMLRegexp `toml:"include"`
	Exclude []TOMLRegexp `toml:"exclude"`
}

type Config struct {
	Server     string         `toml:"server"`
	Namespaces []string       `toml:"namespaces"`
	Images     []WatchedImage `toml:"images"`
}

type TOMLRegexp struct {
	Regexp *regexp.Regexp
}

func (tr *TOMLRegexp) UnmarshalTOML(data interface{}) error {
	rexString, ok := data.(string)
	if !ok {
		return errors.New("value must be a string")
	}

	rex, err := regexp.Compile(rexString)
	if err != nil {
		return err
	}

	tr.Regexp = rex

	return nil
}

func parseConfigFile(path string) (Config, error) {
	var conf Config
	if _, err := toml.DecodeFile(path, &conf); err != nil {
		return Config{}, err
	}

	for i, image := range conf.Images {
		normName, err := reference.ParseNormalizedNamed(image.Name)
		if err != nil {
			return Config{}, err
		}

		conf.Images[i].Name = normName.Name()
	}

	return conf, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	conf, err := parseConfigFile("./config.toml")
	if err != nil {
		return err
	}

	nomadClient, err := api.NewClient(api.DefaultConfig().ClientConfig("", conf.Server, false))
	if err != nil {
		return err
	}

	parsedImageTags, err := getImageVersionMapping(conf.Images)
	if err != nil {
		return err
	}

	instances, err := getAllInstances(nomadClient, conf.Namespaces)
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Namespace", "Job", "Group", "Task", "Image", "Latest", "Current", "UpdateAvailable"})

	for _, instance := range instances {
		versions, ok := parsedImageTags[instance.Image.Name()]
		if !ok {
			continue
		}

		latest := getNewestVersion(versions)
		current, err := version.NewVersion(instance.Image.Tag())
		if err != nil {
			return err
		}
		updateAvailable := latest.GreaterThan(current)

		table.Append([]string{
			instance.Namespace,
			instance.Job,
			instance.Group,
			instance.Task,
			instance.Image.Name(),
			latest.String(),
			current.String(),
			strconv.FormatBool(updateAvailable),
		})
	}
	table.Render()

	return nil
}

func getImageTagMapping(ctx context.Context, images []WatchedImage) (map[string][]string, error) {
	g, ctx := errgroup.WithContext(ctx)
	var mu sync.Mutex

	imageTags := make(map[string][]string)
	for _, watch := range images {
		watch := watch
		g.Go(func() error {
			tags, err := getTags(ctx, watch)
			if err != nil {
				return err
			}

			mu.Lock()
			imageTags[watch.Name] = tags
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return imageTags, nil
}

func getImageVersionMapping(images []WatchedImage) (map[string][]*version.Version, error) {
	imageTags, err := getImageTagMapping(context.Background(), images)
	if err != nil {
		return nil, err
	}

	parsedImageTags := make(map[string][]*version.Version)
	for imageName, tags := range imageTags {
		vers := make([]*version.Version, len(tags))
		for i, tagStr := range tags {
			ver, err := version.NewVersion(tagStr)
			if err != nil {
				return nil, fmt.Errorf("couldn't parse image tag version for %s: %w", imageName, err)
			}
			vers[i] = ver
		}

		parsedImageTags[imageName] = vers
	}

	return parsedImageTags, nil
}

func getTags(ctx context.Context, watched WatchedImage) ([]string, error) {
	repo, err := name.NewRepository(watched.Name)
	if err != nil {
		return nil, err
	}

	scopes := []string{repo.Scope(transport.PullScope)}
	t, err := transport.NewWithContext(ctx, repo.Registry, authn.Anonymous, http.DefaultTransport, scopes)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: t}

	path := fmt.Sprintf("v2/%s/tags/list", repo.RepositoryStr())
	url := fmt.Sprintf("%s://%s/%s", repo.Scheme(), repo.RegistryStr(), path)

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, err
	}

	jsonResp := struct {
		Tags []string `json:"tags"`
	}{}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&jsonResp); err != nil {
		return nil, err
	}

	return filterTags(jsonResp.Tags, watched.Include, watched.Exclude), nil
}

func getNewestVersion(versions []*version.Version) *version.Version {
	var newestVersion *version.Version
	for i, v := range versions {
		if i == 0 {
			newestVersion = v
			continue
		}

		if v.GreaterThan(newestVersion) {
			newestVersion = v
		}
	}

	return newestVersion
}

func getInstances(client *api.Client, namespace string) ([]Instance, error) {
	if namespace == "" {
		namespace = "*"
	}

	opt := api.QueryOptions{
		Namespace:  namespace,
		AllowStale: false,
	}

	allocations := client.Allocations()

	alss, _, err := allocations.List(&opt)
	if err != nil {
		return nil, err
	}

	instances := make([]Instance, 0)
	for _, als := range alss {
		alloc, _, err := allocations.Info(als.ID, &opt)
		if err != nil {
			return nil, err
		}

		tg := alloc.GetTaskGroup()

		jobName := als.JobID
		groupName := tg.Name

		for _, task := range tg.Tasks {
			if task.Driver != "docker" {
				continue
			}

			imageStr := task.Config["image"].(string)
			if strings.HasPrefix(imageStr, "$") {
				continue
			}

			image, err := reference.ParseDockerRef(imageStr)
			if err != nil {
				continue
			}

			instances = append(instances, Instance{
				Namespace: als.Namespace,
				Job:       jobName,
				Group:     *groupName,
				Task:      task.Name,
				Image:     image.(reference.NamedTagged),
			})
		}
	}

	return instances, nil
}

func getAllInstances(client *api.Client, namespaces []string) ([]Instance, error) {
	var allInstances []Instance
	for _, namespace := range namespaces {
		instances, err := getInstances(client, namespace)
		if err != nil {
			return nil, err
		}
		allInstances = append(allInstances, instances...)
	}
	sortInstances(allInstances)
	return allInstances, nil
}

func sortInstances(instances []Instance) {
	less := func(i, j int) bool {
		if instances[i].Namespace != instances[j].Namespace {
			return instances[i].Namespace > instances[j].Namespace
		}

		if instances[i].Job != instances[j].Job {
			return instances[i].Job > instances[j].Job
		}

		if instances[i].Group != instances[j].Group {
			return instances[i].Group > instances[j].Group
		}

		if instances[i].Task != instances[j].Task {
			return instances[i].Task > instances[j].Task
		}

		return false
	}

	sort.Slice(instances, less)
}

func isIncluded(s string, includes []TOMLRegexp) bool {
	if len(includes) == 0 {
		return true
	}
	for _, include := range includes {
		if include.Regexp.MatchString(s) {
			return true
		}
	}
	return false
}

func isExcluded(s string, excludes []TOMLRegexp) bool {
	if len(excludes) == 0 {
		return false
	}
	for _, exclude := range excludes {
		if exclude.Regexp.MatchString(s) {
			return true
		}
	}
	return false
}

func filterTags(tags []string, include, exclude []TOMLRegexp) []string {
	filtered := make([]string, 0)
	for _, tag := range tags {
		if !isIncluded(tag, include) {
			continue
		} else if isExcluded(tag, exclude) {
			continue
		}
		filtered = append(filtered, tag)
	}
	return filtered
}
