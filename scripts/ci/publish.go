package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type release struct {
	TagName         string
	TargetCommitish string
	IsDraft         *bool
	IsPrerelease    *bool
	Assets          []struct{ Name string }
}

func verifyOutput(output, sha, tag string, p profile) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(output, identityFile))
	if err != nil {
		return nil, err
	}
	var identity sourceIdentity
	if err := decodeJSON(data, &identity); err != nil {
		return nil, err
	}
	if identity.Commit != sha || identity.Tag == nil || *identity.Tag != tag || identity.Component != p.Component {
		return nil, errors.New("artifact source identity differs")
	}
	selected, err := selectArtifacts(p, tag)
	if err != nil {
		return nil, err
	}
	expected := make(map[string]bool)
	for _, entry := range selected {
		expected[entry.filename(strings.ReplaceAll(tag, "/", "-"))] = true
	}
	data, err = os.ReadFile(filepath.Join(output, checksumFile))
	if err != nil {
		return nil, err
	}
	var names []string
	records := bufio.NewScanner(strings.NewReader(string(data)))
	for records.Scan() {
		digest, name, ok := strings.Cut(records.Text(), "  ")
		if !ok || !expected[name] {
			return nil, fmt.Errorf("unexpected or duplicate checksum record: %q", name)
		}
		actual, err := fileDigest(filepath.Join(output, name))
		if err != nil {
			return nil, err
		}
		if fmt.Sprintf("%x", actual) != digest {
			return nil, fmt.Errorf("artifact checksum differs: %s", name)
		}
		delete(expected, name)
		names = append(names, name)
	}
	if err := records.Err(); err != nil {
		return nil, err
	}
	directory, err := os.ReadDir(output)
	if err != nil {
		return nil, err
	}
	names = append(names, identityFile, checksumFile)
	if len(expected) != 0 || len(directory) != len(names) {
		return nil, errors.New("artifact set differs from declared namespace targets")
	}
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(output, name))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("expected regular artifact: %s", name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (r runner) requireRemoteTag(ctx context.Context, tag, sha string) error {
	ref := "refs/tags/" + tag
	result, err := r.command(ctx, "git", "ls-remote", "origin", ref, ref+"^{}")
	if err != nil {
		return err
	}
	var direct, peeled string
	for line := range strings.SplitSeq(result, "\n") {
		hash, match, _ := strings.Cut(line, "\t")
		switch match {
		case ref:
			direct = hash
		case ref + "^{}":
			peeled = hash
		}
	}
	if peeled != "" {
		direct = peeled
	}
	if direct != sha {
		return errors.New("source/remote tag commit differs")
	}
	return nil
}

func (r runner) requireMain(ctx context.Context, repository, sha, tag string) error {
	if _, err := r.command(ctx, "git", "fetch", "--no-tags", "origin", "main"); err != nil {
		return err
	}
	if _, err := r.command(ctx, "git", "merge-base", "--is-ancestor", sha, "FETCH_HEAD"); err != nil {
		return err
	}
	workflow := "ci.yml"
	if strings.HasPrefix(tag, "api/") {
		workflow = "api-ci.yml"
	}
	output, err := r.command(ctx, "gh", "run", "list", "--repo", repository, "--branch", "main",
		"--event", "push", "--commit", sha, "--workflow", workflow,
		"--json", "headSha,status,conclusion", "--limit", "20")
	if err != nil {
		return err
	}
	var runs []struct{ HeadSHA, Status, Conclusion string }
	if err := json.Unmarshal([]byte(output), &runs); err != nil {
		return err
	}
	for _, run := range runs {
		if run.HeadSHA == sha && run.Status == "completed" && run.Conclusion == "success" {
			return nil
		}
	}
	return errors.New("required main push CI has not succeeded")
}

func (r runner) publish(ctx context.Context, profile profile, output, tag, sha string) error {
	if !releaseTag.MatchString(tag) || !commitSHA.MatchString(sha) {
		return errors.New("invalid release identity")
	}
	if os.Getenv("GITHUB_EVENT_NAME") != "push" || os.Getenv("GITHUB_REF") != "refs/tags/"+tag {
		return errors.New("publication requires the matching push tag event")
	}
	repository := "endless-net/" + profile.Component
	if os.Getenv("GITHUB_REPOSITORY") != repository {
		return errors.New("repository identity differs from profile")
	}
	head, err := r.cleanCommit(ctx)
	if err != nil {
		return err
	}
	if head != sha {
		return errors.New("checkout differs from release commit")
	}
	output, err = r.outputPath(ctx, output)
	if err != nil {
		return err
	}
	names, err := verifyOutput(output, sha, tag, profile)
	if err != nil {
		return err
	}
	if err := r.requireRemoteTag(ctx, tag, sha); err != nil {
		return err
	}
	if err := r.publishAssets(ctx, output, repository, tag, sha, names); err != nil {
		return err
	}
	fmt.Println("PUBLISHED", repository, tag, sha)
	return nil
}

func (r runner) viewRelease(ctx context.Context, repository, tag string) (release, error) {
	output, err := r.command(ctx, "gh", "release", "view", tag, "--repo", repository, "--json",
		"tagName,targetCommitish,isDraft,isPrerelease,assets")
	if err != nil {
		return release{}, err
	}
	var info release
	err = json.Unmarshal([]byte(output), &info)
	return info, err
}

func (info release) assetNames(tag, sha string, expected []string) ([]string, error) {
	if info.TagName != tag || info.TargetCommitish != sha || info.IsDraft == nil ||
		info.IsPrerelease == nil || *info.IsPrerelease || info.Assets == nil {
		return nil, errors.New("existing release identity differs")
	}
	var names []string
	for _, asset := range info.Assets {
		if !slices.Contains(expected, asset.Name) || slices.Contains(names, asset.Name) {
			return nil, errors.New("existing release asset set differs")
		}
		names = append(names, asset.Name)
	}
	if !*info.IsDraft && len(names) != len(expected) {
		return nil, errors.New("existing published asset set differs")
	}
	return names, nil
}

func (r runner) publishAssets(ctx context.Context, output, repository, tag, sha string, names []string) error {
	info, err := r.viewRelease(ctx, repository, tag)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "release not found") {
			return err
		}
		if err := r.requireMain(ctx, repository, sha, tag); err != nil {
			return err
		}
		if err := r.requireRemoteTag(ctx, tag, sha); err != nil {
			return err
		}
		if _, err := r.command(ctx, "gh", "release", "create", tag, "--repo", repository, "--verify-tag",
			"--draft", "--target", sha, "--title", tag, "--generate-notes"); err != nil {
			return err
		}
		info, err = r.viewRelease(ctx, repository, tag)
		if err != nil {
			return err
		}
	} else if info.IsDraft != nil && *info.IsDraft {
		if err := r.requireMain(ctx, repository, sha, tag); err != nil {
			return err
		}
	}
	present, err := info.assetNames(tag, sha, names)
	if err != nil {
		return err
	}
	if err := r.verifyAssets(ctx, output, repository, tag, present); err != nil {
		return err
	}
	for _, name := range names {
		if !slices.Contains(present, name) {
			if _, err := r.command(ctx, "gh", "release", "upload", tag, filepath.Join(output, name),
				"--repo", repository); err != nil {
				return err
			}
		}
	}
	final, err := r.viewRelease(ctx, repository, tag)
	if err != nil {
		return err
	}
	assets, err := final.assetNames(tag, sha, names)
	if err != nil {
		return err
	}
	if len(assets) != len(names) {
		return errors.New("release changed during upload")
	}
	if err := r.verifyAssets(ctx, output, repository, tag, names); err != nil {
		return err
	}
	if err := r.requireRemoteTag(ctx, tag, sha); err != nil {
		return err
	}
	if *final.IsDraft {
		latest := "true"
		if strings.HasPrefix(tag, "api/") {
			latest = "false"
		}
		_, err := r.command(ctx, "gh", "release", "edit", tag, "--repo", repository, "--draft=false",
			"--prerelease=false", "--latest="+latest)
		return err
	}
	return nil
}

func (r runner) verifyAssets(ctx context.Context, output, repository, tag string, names []string) error {
	dir, err := os.MkdirTemp("", "endlessnet-release-")
	if err != nil {
		return err
	}
	defer cleanup(dir)
	for _, name := range names {
		if _, err := r.command(ctx, "gh", "release", "download", tag, "--repo", repository,
			"--pattern", name, "--dir", dir); err != nil {
			return err
		}
		remote, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		local, err := fileDigest(filepath.Join(output, name))
		if err != nil {
			return err
		}
		if remote != local {
			return fmt.Errorf("release asset bytes differ: %s", name)
		}
	}
	return nil
}
