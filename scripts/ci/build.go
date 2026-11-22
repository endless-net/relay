package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var releaseTag = regexp.MustCompile("^(api/)?v0[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$")
var commitSHA = regexp.MustCompile("^[a-f0-9]{40}$")

const checksumFile = "checksums.txt"
const identityFile = "source.json"

type sourceIdentity struct {
	Component string  `json:"component"`
	Commit    string  `json:"commit"`
	Tag       *string `json:"tag"`
}

type archiveEntry struct {
	name    string
	content []byte
	mode    fs.FileMode
}

func (a artifact) filename(version string) string {
	target := a.Target
	if a.Kind == "source" {
		target.OS, target.Arch = "source", "any"
	}
	extension := ".tar.gz"
	if target.OS == "windows" {
		extension = ".zip"
	}
	return a.Name + "-" + version + "-" + target.OS + "-" + target.Arch + extension
}

func selectArtifacts(p profile, tag string) ([]artifact, error) {
	if tag == "" {
		return p.Artifacts, nil
	}
	if !releaseTag.MatchString(tag) {
		return nil, errors.New("only ordinary v0 tags can be released")
	}
	kind := "runtime"
	if strings.HasPrefix(tag, "api/") {
		kind = "api"
	}
	var selected []artifact
	for _, entry := range p.Artifacts {
		if entry.Release == kind {
			selected = append(selected, entry)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("selected release has no artifacts")
	}
	return selected, nil
}

func (r runner) cleanCommit(ctx context.Context) (string, error) {
	checkout, err := r.command(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	checkout, err = filepath.EvalSymlinks(filepath.FromSlash(checkout))
	if err != nil {
		return "", err
	}
	if checkout != r.root {
		return "", errors.New("source must be the root of its committed Git checkout")
	}
	sha, err := r.command(ctx, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	status, err := r.command(ctx, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if status != "" || !commitSHA.MatchString(sha) {
		return "", errors.New("source checkout must be clean and have a valid commit")
	}
	return sha, nil
}

func resolveOutput(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("output path cannot be resolved: %s", path)
	}
	parent, err := resolveOutput(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func (r runner) outputPath(ctx context.Context, output string) (string, error) {
	output, err := resolveOutput(output)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(r.root, output)
	if err != nil {
		if filepath.VolumeName(r.root) != filepath.VolumeName(output) {
			return output, nil
		}
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return output, nil
	}
	if relative == "." || slices.Contains(strings.Split(filepath.ToSlash(relative), "/"), ".git") {
		return "", errors.New("build output must be outside source or in an ignored directory")
	}
	_, err = r.command(ctx, "git", "check-ignore", "--quiet", "--", filepath.ToSlash(relative)+"/")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", errors.New("build output inside source must be ignored by Git")
	}
	return output, err
}

func (r runner) build(ctx context.Context, p profile, output, tag string) error {
	selected, err := selectArtifacts(p, tag)
	if err != nil {
		return err
	}
	sha, err := r.cleanCommit(ctx)
	if err != nil {
		return err
	}
	output, err = r.outputPath(ctx, output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	directory, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if len(directory) != 0 {
		return errors.New("artifact output must be empty")
	}
	version := sha[:12]
	identity := sourceIdentity{Component: p.Component, Commit: sha}
	if tag != "" {
		version = strings.ReplaceAll(tag, "/", "-")
		identity.Tag = &tag
	}
	var names []string
	for _, entry := range selected {
		files, err := r.buildEntries(ctx, entry, sha)
		if err != nil {
			return err
		}
		name := entry.filename(version)
		if err := writeArchive(filepath.Join(output, name), files); err != nil {
			return err
		}
		names = append(names, name)
		fmt.Println("BUILT", name)
	}
	current, err := r.cleanCommit(ctx)
	if err != nil {
		return err
	}
	if current != sha {
		return errors.New("source commit changed during build")
	}
	slices.Sort(names)
	var checksums strings.Builder
	for _, name := range names {
		digest, err := fileDigest(filepath.Join(output, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%x  %s\n", digest, name)
	}
	if err := os.WriteFile(filepath.Join(output, checksumFile), []byte(checksums.String()), 0o644); err != nil {
		return err
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, identityFile), append(data, '\n'), 0o644)
}

func (r runner) buildEntries(ctx context.Context, a artifact, sha string) ([]archiveEntry, error) {
	if a.Kind == "source" {
		return r.committedEntries(ctx, a.Path, sha)
	}
	tmp, err := os.MkdirTemp("", "endlessnet-ci-")
	if err != nil {
		return nil, err
	}
	defer cleanup(tmp)
	name := a.Name
	if a.Target.OS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(tmp, name)
	env := goEnvironment("GOOS="+a.Target.OS, "GOARCH="+a.Target.Arch, "CGO_ENABLED=0")
	if _, err := r.run(ctx, filepath.Join(r.root, a.Module), env,
		"go", "build", "-trimpath", "-buildvcs=false", "-o", binary, a.Package); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(binary)
	return []archiveEntry{{name: name, content: content, mode: 0o755}}, err
}

func cleanup(path string) {
	if err := os.RemoveAll(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (r runner) committedEntries(ctx context.Context, source, sha string) ([]archiveEntry, error) {
	source = path.Clean(source)
	tree, err := r.run(ctx, r.root, nil, "git", "--literal-pathspecs", "ls-tree", "-rz", sha, "--", source)
	if err != nil {
		return nil, err
	}
	var files []archiveEntry
	for record := range strings.SplitSeq(strings.TrimSuffix(string(tree), "\x00"), "\x00") {
		if record == "" {
			continue
		}
		metadata, name, found := strings.Cut(record, "\t")
		mode, kind, object := strings.Cut(metadata, " ")
		objectType, objectSHA, valid := strings.Cut(kind, " ")
		if !found || !object || !valid || objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf("source symlinks/submodules are not bundle files: %s", record)
		}
		if source != "." {
			name = strings.TrimPrefix(name, source+"/")
		}
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") || name == "." {
			return nil, fmt.Errorf("invalid archive member: %s", name)
		}
		data, err := r.run(ctx, r.root, nil, "git", "cat-file", "blob", objectSHA)
		if err != nil {
			return nil, err
		}
		permissions := fs.FileMode(0o644)
		if mode == "100755" {
			permissions = 0o755
		}
		files = append(files, archiveEntry{name: name, content: data, mode: permissions})
	}
	if len(files) == 0 {
		return nil, errors.New("source bundle has no tracked files")
	}
	return files, nil
}

func writeArchive(name string, entries []archiveEntry) error {
	slices.SortFunc(entries, func(a, b archiveEntry) int { return strings.Compare(a.name, b.name) })
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if strings.HasSuffix(name, ".zip") {
		output := zip.NewWriter(file)
		err = writeZip(output, entries)
		return errors.Join(err, output.Close(), file.Close())
	}
	compressed := gzip.NewWriter(file)
	output := tar.NewWriter(compressed)
	err = writeTar(output, entries)
	return errors.Join(err, output.Close(), compressed.Close(), file.Close())
}

func writeZip(output *zip.Writer, entries []archiveEntry) error {
	for _, entry := range entries {
		header := zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(entry.mode)
		file, err := output.CreateHeader(&header)
		if err != nil {
			return err
		}
		if _, err := file.Write(entry.content); err != nil {
			return err
		}
	}
	return nil
}

func writeTar(output *tar.Writer, entries []archiveEntry) error {
	for _, entry := range entries {
		header := tar.Header{Name: entry.name, Mode: int64(entry.mode), Typeflag: tar.TypeReg,
			Size: int64(len(entry.content)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := output.WriteHeader(&header); err != nil {
			return err
		}
		if _, err := output.Write(entry.content); err != nil {
			return err
		}
	}
	return nil
}

func fileDigest(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	info, err := os.Lstat(path)
	if err != nil {
		return digest, err
	}
	if !info.Mode().IsRegular() {
		return digest, fmt.Errorf("expected regular artifact: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	copy(digest[:], hash.Sum(nil))
	return digest, errors.Join(err, file.Close())
}
