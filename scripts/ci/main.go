package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
)

type runner struct {
	root string
	run  func(context.Context, string, []string, ...string) ([]byte, error)
}

func runCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = dir
	command.Env = append(command.Environ(), env...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s%s", strings.Join(args, " "), err, output, stderr.Bytes())
	}
	if stderr.Len() != 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}
	return output, nil
}

func (r runner) command(ctx context.Context, args ...string) (string, error) {
	output, err := r.run(ctx, r.root, nil, args...)
	return strings.TrimSpace(string(output)), err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "CI FAILED:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected check, build or publish")
	}
	options := flag.NewFlagSet(args[0], flag.ContinueOnError)
	root := options.String("root", ".", "root of the source checkout")
	profilePath := options.String("profile", ".ci/profile.json", "profile relative to the checkout")
	output := options.String("out", ".artifacts", "artifact directory")
	tag := options.String("tag", "", "ordinary v0 release tag")
	sha := options.String("sha", "", "exact release commit")
	goOnly := options.Bool("go-only", false, "use only the declared Go modules and CI checks")
	lintConfig := options.String("lint-config", "", "override the module lint configuration")
	if err := options.Parse(args[1:]); err != nil {
		return err
	}
	if options.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	base, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	profile, err := loadProfile(base, *profilePath)
	if err != nil {
		return fmt.Errorf("load profile: %w", err)
	}
	out := *output
	if !filepath.IsAbs(out) {
		out = filepath.Join(base, out)
	}
	r := runner{root: base, run: runCommand}
	switch args[0] {
	case "check":
		return r.check(ctx, profile, *lintConfig, *goOnly)
	case "build":
		return r.build(ctx, profile, out, *tag)
	case "publish":
		return r.publish(ctx, profile, out, *tag, *sha)
	default:
		return errors.New("expected check, build or publish")
	}
}
