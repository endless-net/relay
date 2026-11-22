package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testTag = "v0.3.4"

var testSHA = strings.Repeat("a", 40)

func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProfile(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "go.mod"), "module example.org/test\n")
	const valid = `{"component":"test","modules":[{"path":".","kind":"go"}],"artifacts":[{"release":"api","name":"test-api","kind":"source","path":"."}]}`
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"normal", valid, true},
		{"duplicate key", strings.Replace(valid, "\"component\"", "\"component\":\"other\",\"component\"", 1), false},
		{"unknown field", strings.Replace(valid, "\"component\"", "\"unknown\":true,\"component\"", 1), false},
		{"escaped artifact", strings.Replace(valid, "\"source\",\"path\":\".\"", "\"source\",\"path\":\"../outside\"", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put(t, filepath.Join(root, "profile.json"), tc.data)
			if _, err := loadProfile(root, "profile.json"); (err == nil) != tc.valid {
				t.Fatalf("loadProfile: %v", err)
			}
		})
	}
	if err := os.Mkdir(filepath.Join(root, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "extra", "go.mod"), "module example.org/extra\n")
	put(t, filepath.Join(root, "profile.json"), valid)
	if _, err := loadProfile(root, "profile.json"); err == nil || !strings.Contains(err.Error(), "owned Go module missing") {
		t.Fatalf("loadProfile with undeclared module: %v", err)
	}
}

func TestReleaseSelection(t *testing.T) {
	p := profile{Artifacts: []artifact{{Release: "api"}, {Release: "runtime"}}}
	for _, tc := range []struct {
		tag     string
		count   int
		release string
	}{
		{"", 2, ""}, {testTag, 1, "runtime"}, {"api/" + testTag, 1, "api"},
		{"v1.0.0", 0, ""}, {"v0.03.0", 0, ""}, {"v0.0.1-rc.1", 0, ""},
	} {
		got, err := selectArtifacts(p, tc.tag)
		if len(got) != tc.count || (err != nil) != (tc.count == 0) ||
			(tc.release != "" && got[0].Release != tc.release) {
			t.Errorf("selectArtifacts(%q) = %v, %v", tc.tag, got, err)
		}
	}
}

func TestSourceBuild(t *testing.T) {
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := runner{root: root, run: runCommand}
	git := func(args ...string) string {
		t.Helper()
		got, err := r.command(ctx, append([]string{"git"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	git("init", "--quiet")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("config", "commit.gpgsign", "false")
	put(t, filepath.Join(root, ".gitignore"), "/.artifacts/\n")
	if err := os.Mkdir(filepath.Join(root, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "api", "document.txt")
	put(t, file, "committed data")
	git("add", ".")
	git("commit", "--quiet", "-m", "fixture")
	sha := git("rev-parse", "HEAD")
	p := profile{Component: "test", Artifacts: []artifact{{Release: "api", Kind: "source", Name: "test-api", Path: "api"}}}
	out := filepath.Join(root, ".artifacts", "first")
	tag := "api/" + testTag
	if err := r.build(ctx, p, out, tag); err != nil {
		t.Fatal(err)
	}
	names, err := verifyOutput(out, sha, tag, p)
	if err != nil || len(names) != 3 {
		t.Fatalf("verifyOutput: %v, %v", names, err)
	}
	second := filepath.Join(root, ".artifacts", "second")
	if err := r.build(ctx, p, second, tag); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		firstHash, firstErr := fileDigest(filepath.Join(out, name))
		secondHash, secondErr := fileDigest(filepath.Join(second, name))
		if firstErr != nil || secondErr != nil || firstHash != secondHash {
			t.Fatalf("%s differs across builds: %v %v", name, firstErr, secondErr)
		}
	}
	if err := r.build(ctx, p, filepath.Join(root, "unignored"), tag); err == nil {
		t.Fatal("accepted unignored output")
	}
	put(t, filepath.Join(out, p.Artifacts[0].filename("api-"+testTag)), "tampered")
	if _, err := verifyOutput(out, sha, tag, p); err == nil {
		t.Fatal("accepted mismatched checksum")
	}
	put(t, file, "dirty data")
	if err := r.build(ctx, p, t.TempDir(), tag); err == nil {
		t.Fatal("accepted dirty checkout")
	}
	files, err := r.committedEntries(ctx, "api", sha)
	if err != nil || len(files) != 1 || files[0].name != "document.txt" || string(files[0].content) != "committed data" {
		t.Fatalf("committed entries: %v %v", files, err)
	}
}

func TestFirstPartyReplace(t *testing.T) {
	r := runner{run: func(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
		if !slices.Equal(args, []string{"go", "mod", "edit", "-json"}) ||
			!slices.Contains(env, "GOFLAGS=-mod=readonly") {
			t.Fatalf("unexpected check: %v (%v)", args, env)
		}
		return []byte(`{"Replace":[{"Old":{"Path":"github.com/endless-net/example"}}]}`), nil
	}}
	if err := r.checkGo(t.Context(), ".", "lint.yml", false); err == nil || !strings.Contains(err.Error(), "first-party replace") {
		t.Fatalf("checkGo: %v", err)
	}
}

func TestRequireMain(t *testing.T) {
	for _, tc := range []struct {
		name, sha, status, conclusion string
		ancestry                      bool
		wantError                     bool
	}{
		{"exact successful push", testSHA, "completed", "success", true, false},
		{"different commit", strings.Repeat("b", 40), "completed", "success", true, true},
		{"failure", testSHA, "completed", "failure", true, true},
		{"missing ancestry", testSHA, "completed", "success", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runner{run: func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
				switch args[1] {
				case "fetch":
					return nil, nil
				case "merge-base":
					if !tc.ancestry {
						return nil, errors.New("not an ancestor")
					}
					return nil, nil
				case "run":
					if !tc.ancestry || !slices.Contains(args, "push") || !slices.Contains(args, testSHA) {
						t.Fatalf("unexpected query: %v", args)
					}
					return json.Marshal([]map[string]string{{"headSha": tc.sha, "status": tc.status, "conclusion": tc.conclusion}})
				default:
					t.Fatalf("unexpected command: %v", args)
					return nil, nil
				}
			}}
			if err := r.requireMain(t.Context(), "endless-net/test", testSHA, testTag); (err != nil) != tc.wantError {
				t.Fatalf("requireMain: %v", err)
			}
		})
	}
}

func TestPublishRejectsWrongEvent(t *testing.T) {
	t.Setenv("GITHUB_EVENT_NAME", "pull_request")
	t.Setenv("GITHUB_REF", "refs/tags/"+testTag)
	r := runner{run: func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
		t.Fatalf("unexpected command: %v", args)
		return nil, nil
	}}
	err := r.publish(t.Context(), profile{Component: "test"}, t.TempDir(), testTag, testSHA)
	if err == nil || !strings.Contains(err.Error(), "matching push tag event") {
		t.Fatalf("publish: %v", err)
	}
}

func TestPublishAssets(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		initial                       map[string]string
		missing, draft, corruptUpload bool
		want                          []string
		wantError                     bool
	}{
		{"published retry", map[string]string{"first": "one", "second": "two"}, false, false, false, nil, false},
		{"partial draft", map[string]string{"first": "one"}, false, true, false, []string{"upload second", "edit"}, false},
		{"conflicting asset", map[string]string{"first": "wrong"}, false, true, false, nil, true},
		{"corrupt upload", map[string]string{"first": "one"}, false, true, true, []string{"upload second"}, true},
		{"new draft", map[string]string{}, true, true, false, []string{"create", "upload first", "upload second", "edit"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := t.TempDir()
			put(t, filepath.Join(output, "first"), "one")
			put(t, filepath.Join(output, "second"), "two")
			var mutations []string
			missing, draft := tc.missing, tc.draft
			r := runner{run: func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
				if args[0] == "git" {
					if args[1] == "fetch" || args[1] == "merge-base" {
						if !tc.draft {
							t.Fatalf("published retry should only verify existing assets: %v", args)
						}
						return nil, nil
					}
					return []byte(testSHA + "\trefs/tags/" + testTag), nil
				}
				if args[1] == "run" {
					return json.Marshal([]map[string]string{{"headSha": testSHA, "status": "completed", "conclusion": "success"}})
				}
				switch args[2] {
				case "view":
					if missing {
						return nil, errors.New("release not found")
					}
					assets := make([]map[string]string, 0, len(tc.initial))
					for name := range tc.initial {
						assets = append(assets, map[string]string{"name": name})
					}
					return json.Marshal(map[string]any{"tagName": testTag, "targetCommitish": testSHA,
						"isDraft": draft, "isPrerelease": false, "assets": assets})
				case "download":
					name := args[slices.Index(args, "--pattern")+1]
					dir := args[slices.Index(args, "--dir")+1]
					put(t, filepath.Join(dir, name), tc.initial[name])
				case "create":
					mutations = append(mutations, "create")
					missing = false
				case "upload":
					name := filepath.Base(args[4])
					mutations = append(mutations, "upload "+name)
					data, err := os.ReadFile(args[4])
					if tc.corruptUpload {
						data = []byte("corrupted")
					}
					tc.initial[name] = string(data)
					return nil, err
				case "edit":
					mutations = append(mutations, "edit")
					if !slices.Contains(args, "--latest=true") {
						t.Fatal("runtime must be latest")
					}
					draft = false
				default:
					t.Fatalf("unexpected command: %v", args)
				}
				return nil, nil
			}}
			err := r.publishAssets(t.Context(), output, "endless-net/test", testTag, testSHA, []string{"first", "second"})
			if (err != nil) != tc.wantError || !slices.Equal(mutations, tc.want) {
				t.Fatalf("publishAssets: %v, mutations %v, want %v", err, mutations, tc.want)
			}
		})
	}
}
