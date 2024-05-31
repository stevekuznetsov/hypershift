package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"
)

type options struct {
	quayHost  string
	quayToken string

	firstCommit string
	remote      string

	outputDir string
}

func defaultOptions() *options {
	return &options{
		quayHost: "https://quay.io",
		remote:   "origin",
	}
}

func bindOptions(opts *options, flags *pflag.FlagSet) {
	flags.StringVar(&opts.firstCommit, "first-commit", opts.firstCommit, "The oldest commit for which we search for an image tag.")
	flags.StringVar(&opts.remote, "remote", opts.remote, "The name of the remote from which branches are fetched.")
	flags.StringVar(&opts.quayHost, "quay-host", opts.quayHost, "Host for the Quay instance to query.")
	flags.StringVar(&opts.quayToken, "quay-token", opts.quayToken, "Bearer token to authenticate with the Quay API.")
	flags.StringVar(&opts.outputDir, "output-dir", opts.outputDir, "Directory to use for caching data and outputting analysis.")
}

func (o *options) Validate() error {
	if o.quayToken == "" {
		return errors.New("--quay-token is required")
	}
	if o.outputDir == "" {
		return errors.New("--output-dir is required")
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		cancel()
	}()
	opts := defaultOptions()
	bindOptions(opts, pflag.CommandLine)
	pflag.Parse()
	if err := opts.Validate(); err != nil {
		log.Fatal(err)
	}

	tags, err := publishedTags(ctx, opts.quayHost, opts.quayToken, opts.outputDir)
	if err != nil {
		log.Fatal(err)
	}

	commits, err := mergeCommits(ctx, opts.firstCommit, opts.remote)
	if err != nil {
		log.Fatal(err)
	}

	if err := summarize(tags, commits, opts.outputDir); err != nil {
		log.Fatal(err)
	}
}

type tagsOutput struct {
	HasAdditional bool        `json:"has_additional"`
	Page          int         `json:"page"`
	Tags          []tagOutput `json:"tags"`
}

type tagOutput struct {
	Name         string `json:"name"`
	LastModified string `json:"last_modified"`
}

const hyperShiftRepo = "acm-d/rhtap-hypershift-operator"

func publishedTags(ctx context.Context, quayHost, quayToken string, outputDir string) (map[string]time.Time, error) {
	tagsDir := filepath.Join(outputDir, "tags")
	if err := os.MkdirAll(tagsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create dir for tags: %w", err)
	}
	tags := map[string]time.Time{}
	page := 1
	oldest := time.Now()
	for {
		var rawPage []byte
		pagePath := filepath.Join(tagsDir, fmt.Sprintf("%d.json", page))
		if _, err := os.Stat(pagePath); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to stat tags file %s: %w", pagePath, err)
			}
			// if we don't have the file, we just need to fetch it
			log.Printf("fetching tags page %d from the API", page)
			rawPage, err = fetchTags(ctx, quayHost, quayToken, page)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch tags page %d from the API: %w", page, err)
			}

			if err := os.WriteFile(pagePath, rawPage, 0644); err != nil {
				return nil, fmt.Errorf("failed to write tags page %d to the API: %w", page, err)
			}
		} else {
			// if we have a file, we can just load it
			log.Printf("fetching tags page %d from disk", page)
			var loadErr error
			rawPage, loadErr = os.ReadFile(pagePath)
			if loadErr != nil {
				return nil, fmt.Errorf("failed to read tags file %s: %w", pagePath, loadErr)
			}
		}
		var pageData tagsOutput
		if err := json.Unmarshal(rawPage, &pageData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tags file %s: %w", pagePath, err)
		}

		if pageData.Page != page {
			return nil, fmt.Errorf("tags file %s has wrong page, expected %d, got %d", pagePath, page, pageData.Page)
		}

		for _, tag := range pageData.Tags {
			if len(tag.Name) != 40 {
				continue
			}

			modifiedTime, err := time.Parse(time.RFC1123Z, tag.LastModified)
			if err != nil {
				return nil, fmt.Errorf("failed to parse modification time on tag %s: %w", tag.Name, err)
			}
			tags[tag.Name] = modifiedTime

			if modifiedTime.Before(oldest) {
				oldest = modifiedTime
			}
		}

		if pageData.HasAdditional {
			page++
			continue
		}
		break
	}
	return tags, nil
}

func fetchTags(ctx context.Context, quayHost, quayToken string, page int) ([]byte, error) {
	uri, err := url.Parse(quayHost + "/api/v1/repository/" + hyperShiftRepo + "/tag")
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}
	query := uri.Query()
	query.Add("page", strconv.Itoa(page))
	uri.RawQuery = query.Encode()

	log.Printf("Fetching url %s", uri.String())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+quayToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("failed to close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch tags: %s: %s", resp.Status, string(body))
	}

	return body, nil
}

type commitInfo struct {
	sha  string
	date time.Time
}

func mergeCommits(ctx context.Context, firstCommit, remote string) ([]commitInfo, error) {
	var commits []commitInfo
	seen := sets.Set[string]{}
	for _, branch := range []string{
		"main",
		"release-4.18",
		"release-4.17",
		"release-4.16",
		"release-4.15",
		"release-4.14",
		"release-4.13",
		"main-0.1.16-rehearsal-hotfix",
	} {
		previous := len(commits)
		log.Printf("fetching commits for branch %s", branch)
		args := []string{
			"log",
			"--merges",
			"--pretty=format:%H\u00A0%ad",
			"--date=iso8601-strict",
		}
		if firstCommit != "" {
			args = append(args, firstCommit+"^1..."+remote+"/"+branch)
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to run git %s: %s, %s", strings.Join(args, " "), stdout.String(), stderr.String())
		}

		for _, line := range strings.Split(stdout.String(), "\n") {
			line = strings.TrimSpace(line)
			parts := strings.Split(line, "\u00A0")
			if len(parts) != 2 {
				return nil, fmt.Errorf("incorrect parts from git output: %q", line)
			}
			commitSha, rawCommittedTime := parts[0], parts[1]
			if seen.Has(commitSha) {
				continue
			}
			committedTime, err := time.Parse(time.RFC3339, rawCommittedTime)
			if err != nil {
				return nil, fmt.Errorf("invalid time %s: %w", rawCommittedTime, err)
			}
			commits = append(commits, commitInfo{
				sha:  commitSha,
				date: committedTime,
			})
			seen.Insert(commitSha)
		}
		log.Printf("fetched %d commits for branch %s", len(commits)-previous, branch)
	}
	return commits, nil
}

type summary struct {
	// Commit is the commit SHA.
	Commit string `json:"commit"`

	// Date is the commit date formatted in ISO8601 format.
	Date string `json:"date"`
	date time.Time

	// Published exposes whether the commit was published into an image tag.
	Published bool `json:"published"`

	// PublishedTime is the publication date of the image tag in ISO8601 format.
	PublishedTime string `json:"publishedTime,omitempty"`
	publishedTime time.Time
}

func summarize(tags map[string]time.Time, commits []commitInfo, outputDir string) error {
	var summaries []summary
	for _, commit := range commits {
		date, published := tags[commit.sha]
		s := summary{
			Commit:    commit.sha,
			Date:      commit.date.Format(time.RFC3339),
			date:      commit.date,
			Published: published,

			publishedTime: date,
		}
		if published {
			s.PublishedTime = date.Format(time.RFC3339)
		}
		summaries = append(summaries, s)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].date.Before(summaries[j].date)
	})

	raw, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Errorf("failed to marshal summary: %w", err)
	}

	output := filepath.Join(outputDir, "summary.json")
	if err := os.WriteFile(output, raw, 0644); err != nil {
		return fmt.Errorf("failed to write summary.json: %w", err)
	}

	log.Printf("wrote %d summaries to %s", len(summaries), output)
	return nil
}
