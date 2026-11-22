package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var identifier = regexp.MustCompile("^[a-z][a-z0-9-]*$")
var goPackage = regexp.MustCompile("^[.]/[a-zA-Z0-9_./-]+$")

type profile struct {
	Schema    int
	Component string
	Modules   []module
	Artifacts []artifact
}

type module struct {
	Path string
	Kind string
	Unit []string
}

type target struct {
	OS   string
	Arch string
}

type artifact struct {
	Release string
	Kind    string
	Name    string
	Module  string
	Package string
	Path    string
	Target  target
}

func decodeJSON(data []byte, out any) error {
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	if err := uniqueKeys(tokens); err != nil {
		return err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return errors.New("unexpected data after JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func uniqueKeys(tokens *json.Decoder) error {
	token, err := tokens.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	keys := make(map[string]bool)
	for tokens.More() {
		if delimiter == '{' {
			token, err := tokens.Token()
			if err != nil {
				return err
			}
			key := token.(string)
			if keys[key] {
				return fmt.Errorf("duplicate JSON key: %s", key)
			}
			keys[key] = true
		}
		if err := uniqueKeys(tokens); err != nil {
			return err
		}
	}
	_, err = tokens.Token()
	return err
}

func localPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, "\\:") ||
		strings.HasPrefix(name, "/") || slices.Contains(strings.Split(name, "/"), "..") {
		return "", fmt.Errorf("expected checkout-relative path: %q", name)
	}
	path := root
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink source path is forbidden: %s", path)
		}
	}
	return path, nil
}

func loadProfile(root, name string) (profile, error) {
	path, err := localPath(root, name)
	if err != nil {
		return profile{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return profile{}, err
	}
	result := profile{Schema: 1}
	if err := decodeJSON(data, &result); err != nil {
		return profile{}, err
	}
	if result.Schema != 1 || !identifier.MatchString(result.Component) ||
		len(result.Modules) == 0 || len(result.Artifacts) == 0 {
		return profile{}, errors.New("invalid profile identity or missing modules/artifacts")
	}
	return result, result.validate(root)
}

func (p profile) validate(root string) error {
	modules := make(map[string]module)
	for _, entry := range p.Modules {
		path, err := localPath(root, entry.Path)
		if err != nil {
			return err
		}
		if _, exists := modules[path]; exists {
			return fmt.Errorf("duplicate module: %s", entry.Path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("module must be a directory: %s", entry.Path)
		}
		switch entry.Kind {
		case "go":
			manifest, err := localPath(path, "go.mod")
			if err != nil {
				return err
			}
			info, err := os.Stat(manifest)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || len(entry.Unit) != 0 {
				return fmt.Errorf("invalid Go module: %s", entry.Path)
			}
		default:
			return fmt.Errorf("unsupported module kind: %s", entry.Kind)
		}
		modules[path] = entry
	}
	if err := checkOwnedModules(root, modules); err != nil {
		return err
	}
	names := make(map[string]bool)
	for _, entry := range p.Artifacts {
		if (entry.Release != "runtime" && entry.Release != "api") || !identifier.MatchString(entry.Name) {
			return fmt.Errorf("invalid release artifact: %s", entry.Name)
		}
		if err := entry.validate(root, modules); err != nil {
			return err
		}
		name := entry.filename("build")
		if names[name] {
			return fmt.Errorf("duplicate artifact output: %s", name)
		}
		names[name] = true
	}
	return nil
}

func checkOwnedModules(root string, modules map[string]module) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch entry.Name() {
		case ".git", ".artifacts", ".dart_tool", "build", "vendor", ".venv":
			if path != root && entry.IsDir() {
				return filepath.SkipDir
			}
		case "go.mod":
			if modules[filepath.Dir(path)].Kind != "go" {
				return fmt.Errorf("owned Go module missing from profile: %s", path)
			}
		case "pubspec.yaml":
			return fmt.Errorf("Dart/Flutter module has no supported CI entrypoint: %s", path)
		}
		return nil
	})
}

func (a artifact) validate(root string, modules map[string]module) error {
	switch a.Kind {
	case "source":
		if a.Module != "" || a.Package != "" || a.Target != (target{}) {
			return errors.New("unexpected source artifact field")
		}
		_, err := localPath(root, a.Path)
		return err
	case "go":
		if a.Path != "" || !goPackage.MatchString(a.Package) ||
			!slices.Contains([]string{"linux", "windows", "darwin"}, a.Target.OS) ||
			!slices.Contains([]string{"amd64", "arm64"}, a.Target.Arch) {
			return errors.New("invalid Go target/package")
		}
		path, err := localPath(root, a.Module)
		if err != nil {
			return err
		}
		if modules[path].Kind != "go" {
			return errors.New("artifact Go module is not checked")
		}
		_, err = localPath(path, a.Package)
		return err
	default:
		return fmt.Errorf("unsupported artifact kind: %s", a.Kind)
	}
}
