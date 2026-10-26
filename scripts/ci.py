#!/usr/bin/env python3
"""Autonomous public producer CI. No private Service Kit dependency."""
from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import zipfile

TAG = re.compile(r"(?:api/)?v0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
MODULE_KINDS = {"go", "dart", "flutter", "json"}
ARTIFACT_KINDS = {"go", "source", "flutter-web", "flutter-desktop"}


def strict_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key: " + key)
        result[key] = value
    return result


def load_json(path):
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=strict_pairs)


def command(args, cwd=None, capture=False, env=None):
    result = subprocess.run(args, cwd=cwd, env=env, check=True,
                            stdout=subprocess.PIPE if capture else None,
                            text=capture)
    return result.stdout.strip() if capture else ""


def local(root, value):
    if (not isinstance(value, str) or not value or Path(value).is_absolute()
            or "\\" in value or ":" in value or ".." in Path(value).parts):
        raise ValueError("expected relative path")
    candidate = (root / value).resolve()
    if candidate != Path(os.path.abspath(root / value)):
        raise ValueError("symlink source path is forbidden")
    if not candidate.is_relative_to(root.resolve()):
        raise ValueError("path escapes checkout")
    if not candidate.exists():
        raise ValueError("missing path: " + value)
    return candidate


def load_profile(root, path):
    data = load_json(local(root, path))
    if set(data) - {"schema", "component", "modules", "artifacts"}:
        raise ValueError("unknown profile field")
    if data.get("schema", 1) != 1 or not re.fullmatch(r"[a-z][a-z0-9-]*", data["component"]):
        raise ValueError("invalid profile identity")
    if not data.get("modules") or not data.get("artifacts"):
        raise ValueError("modules/artifacts must be explicit")
    modules = set()
    for module in data["modules"]:
        if set(module) - {"path", "kind", "unit"} or module["kind"] not in MODULE_KINDS:
            raise ValueError("unknown module kind/field")
        where = local(root, module["path"])
        if not where.is_dir() or module["path"] in modules:
            raise ValueError("invalid/duplicate module")
        modules.add(module["path"])
        required = {"go": "go.mod", "dart": "pubspec.yaml", "flutter": "pubspec.yaml"}
        if module["kind"] in required and not (where / required[module["kind"]]).is_file():
            raise ValueError("declared module manifest is missing")
        for unit in module.get("unit", []):
            if not isinstance(unit, str) or Path(unit).suffix not in {".dart", ".json", ".py"}:
                raise ValueError("invalid unit entrypoint")
            local(where, unit)
    present = set()
    ignored = {".git", ".artifacts", ".dart_tool", "build", "vendor", ".venv"}
    for manifest in root.rglob("go.mod"):
        relative = manifest.relative_to(root)
        if not any(part in ignored for part in relative.parts):
            present.add(manifest.parent.relative_to(root).as_posix())
    expected = {m["path"] for m in data["modules"] if m["kind"] == "go"}
    if present != expected:
        raise ValueError("every owned Go module must be declared")
    packages = {manifest.parent.relative_to(root).as_posix()
                for manifest in root.rglob("pubspec.yaml")
                if not any(part in ignored for part in manifest.relative_to(root).parts)}
    expected_packages = {m["path"] for m in data["modules"] if m["kind"] in {"dart", "flutter"}}
    if packages != expected_packages:
        raise ValueError("every owned Dart/Flutter package must be declared")
    outputs = set()
    for artifact in data["artifacts"]:
        if set(artifact) - {"release", "kind", "name", "module", "package", "path", "target"}:
            raise ValueError("unknown artifact field")
        if artifact.get("release") not in {"runtime", "api"} or artifact["kind"] not in ARTIFACT_KINDS:
            raise ValueError("invalid release/artifact kind")
        if not re.fullmatch(r"[a-z][a-z0-9-]*", artifact["name"]):
            raise ValueError("invalid artifact name")
        if artifact["kind"] == "source":
            local(root, artifact["path"])
        else:
            local(root, artifact["module"])
            if artifact["module"] not in modules:
                raise ValueError("artifact module is not checked")
            target = artifact["target"]
            if set(target) != {"os", "arch"}:
                raise ValueError("invalid target")
            if artifact["kind"] == "go":
                if (target["os"], target["arch"]) not in {
                    ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64"),
                    ("windows", "arm64"), ("darwin", "amd64"), ("darwin", "arm64")
                } or not re.fullmatch(r"\./[a-zA-Z0-9_./-]+", artifact["package"]):
                    raise ValueError("invalid Go target/package")
                local(local(root, artifact["module"]), artifact["package"])
        key = (artifact["release"], artifact["name"],
               json.dumps(artifact.get("target"), sort_keys=True))
        if key in outputs:
            raise ValueError("duplicate artifact")
        outputs.add(key)
    return data


def go_environment():
    env = dict(os.environ, GOWORK="off", GOFLAGS="-mod=readonly")
    return env


def json_cases(where, manifest):
    from jsonschema import Draft202012Validator, FormatChecker
    from referencing import Registry, Resource
    registry = Registry()
    schemas = {}
    for path in (where / "schemas").glob("*.json"):
        schema = load_json(path)
        Draft202012Validator.check_schema(schema)
        schemas[path.relative_to(where).as_posix()] = schema
        if "$id" in schema:
            registry = registry.with_resource(schema["$id"], Resource.from_contents(schema))
    cases = load_json(local(where, manifest))
    if not cases:
        raise ValueError("empty fixture manifest")
    for case in cases:
        source, _, fragment = case["schema"].partition("#")
        schema = schemas.get(source)
        if schema is None:
            raise ValueError("schema not declared locally: " + source)
        reference = schema.get("$id", local(where, source).as_uri())
        registry = registry.with_resource(reference, Resource.from_contents(schema))
        validator = Draft202012Validator(
            {"$ref": reference + ("#" + fragment if fragment else "")},
            registry=registry, format_checker=FormatChecker())
        instance = load_json(local(where, case["instance"]))
        valid = not list(validator.iter_errors(instance))
        if type(case["valid"]) is not bool or valid != case["valid"]:
            raise ValueError("fixture outcome mismatch: " + case["name"])
    print("JSON unit cases:", len(cases), flush=True)


def check(root, profile, lint_config=None):
    for module in profile["modules"]:
        where = local(root, module["path"])
        kind = module["kind"]
        print("CHECK", kind, module["path"], flush=True)
        if kind == "go":
            env = go_environment()
            metadata = json.loads(command(["go", "mod", "edit", "-json"],
                                          cwd=where, capture=True, env=env))
            if any(entry["Old"]["Path"].startswith("github.com/endless-net/")
                   for entry in metadata.get("Replace") or []):
                raise ValueError("first-party replace is forbidden")
            for args in (
                ["goimports", "-l", "."],
            ):
                if command(args, cwd=where, capture=True, env=env):
                    raise ValueError("Go source formatting differs")
            command(["go", "vet", "./..."], cwd=where, env=env)
            config = Path(lint_config).resolve() if lint_config else where / ".golangci-lint.yaml"
            command(["golangci-lint", "run", "--config", str(config), "./...", "--timeout", "1m"], cwd=where, env=env)
            command(["go", "test", "-short", "./..."], cwd=where, env=env)
            command(["govulncheck", "./..."], cwd=where, env=env)
        elif kind in {"dart", "flutter"}:
            sdk = "flutter" if kind == "flutter" else "dart"
            command([sdk, "pub", "get", "--enforce-lockfile"], cwd=where)
            command(["dart", "format", "--output=none", "--set-exit-if-changed", "."], cwd=where)
            command([sdk, "analyze"], cwd=where)
            units = module.get("unit", [])
            if units:
                command([sdk, "test", *units], cwd=where)
            else:
                print("UNIT CASES ABSENT: skeleton/tooling; no coverage claim", flush=True)
        else:
            units = module.get("unit", [])
            if not units:
                raise ValueError("JSON module requires real fixture entrypoints")
            for unit in units:
                if unit.endswith(".py"):
                    command([sys.executable, "-B", str(local(where, unit))], cwd=where)
                else:
                    json_cases(where, unit)


def archive_bytes(root, artifact):
    """Return a file map from committed source only; never archive local caches."""
    source = artifact["path"]
    tracked = subprocess.check_output(["git", "ls-tree", "-r", "--name-only", "HEAD", "--", source],
                                      cwd=root).decode().splitlines()
    if not tracked:
        raise ValueError("source bundle has no tracked files")
    files = {}
    prefix = "" if source == "." else source.rstrip("/") + "/"
    for name in tracked:
        if not name.startswith(prefix):
            raise ValueError("source archive path mismatch")
        mode = command(["git", "ls-tree", "HEAD", "--", name], cwd=root, capture=True).split()[0]
        if mode != "100644" and mode != "100755":
            raise ValueError("source symlinks/submodules are not bundle files")
        content = subprocess.check_output(["git", "show", "HEAD:" + name], cwd=root)
        files[name[len(prefix):]] = (content, 0o755 if mode == "100755" else 0o644)
    return files


def write_archive(path, entries):
    if path.suffix == ".zip":
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as output:
            for name, (content, mode) in sorted(entries.items()):
                info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
                info.external_attr = (0o100000 | mode) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                output.writestr(info, content)
    else:
        with path.open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(mode="w", fileobj=compressed) as output:
                    for name, (content, mode) in sorted(entries.items()):
                        info = tarfile.TarInfo(name)
                        info.size, info.mode, info.mtime = len(content), mode, 0
                        info.uid = info.gid = 0
                        output.addfile(info, io.BytesIO(content))


def clean_commit(root):
    checkout = Path(command(["git", "rev-parse", "--show-toplevel"], cwd=root, capture=True)).resolve()
    if checkout != root.resolve():
        raise ValueError("source must be the root of its committed Git checkout")
    sha = command(["git", "rev-parse", "--verify", "HEAD^{commit}"], cwd=root, capture=True)
    if command(["git", "status", "--porcelain=v1", "--untracked-files=all"], cwd=root, capture=True):
        raise ValueError("source checkout must be clean: tracked or untracked inputs changed")
    return sha


def validate_output_location(root, output):
    output = output.resolve()
    if output.is_relative_to(root.resolve()):
        relative = output.relative_to(root.resolve())
        if relative == Path(".") or ".git" in relative.parts:
            raise ValueError("build output must be outside source or in an ignored directory")
        try:
            command(["git", "check-ignore", "--quiet", "--", relative.as_posix() + "/"], cwd=root)
        except subprocess.CalledProcessError as error:
            if error.returncode != 1:
                raise
            raise ValueError("build output inside source must be ignored by Git") from error
    return output


def build(root, profile, output, tag=None):
    if tag and not TAG.fullmatch(tag):
        raise ValueError("only ordinary v0 tags can be released")
    subset = "api" if tag and tag.startswith("api/") else "runtime"
    selected = [a for a in profile["artifacts"] if not tag or a["release"] == subset]
    if not selected:
        raise ValueError("selected release has no artifacts")
    sha = clean_commit(root)
    output = validate_output_location(root, output)
    version = tag.replace("/", "-") if tag else sha[:12]
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("artifact output must be empty")
    for artifact in selected:
        kind = artifact["kind"]
        target = artifact.get("target", {"os": "source", "arch": "any"})
        name = artifact["name"] + "-" + version + "-" + target["os"] + "-" + target["arch"]
        extension = ".zip" if target["os"] == "windows" else ".tar.gz"
        destination = output / (name + extension)
        if kind == "source":
            entries = archive_bytes(root, artifact)
        elif kind == "go":
            executable = artifact["name"] + (".exe" if target["os"] == "windows" else "")
            with tempfile.TemporaryDirectory() as temporary:
                binary = Path(temporary) / executable
                env = dict(go_environment(), GOOS=target["os"], GOARCH=target["arch"], CGO_ENABLED="0")
                command(["go", "build", "-trimpath", "-buildvcs=false", "-o", str(binary),
                         artifact["package"]], cwd=local(root, artifact["module"]), env=env)
                entries = {executable: (binary.read_bytes(), 0o755)}
        else:
            raise ValueError("Flutter native packaging is supplied by its native workflow")
        write_archive(destination, entries)
        print("BUILT", destination.name, flush=True)
    if clean_commit(root) != sha:
        raise ValueError("source commit changed during build")
    names = sorted(p.name for p in output.iterdir())
    checksum_lines = [hashlib.sha256((output / name).read_bytes()).hexdigest() + "  " + name
                      for name in names]
    (output / "checksums.txt").write_text("\n".join(checksum_lines) + "\n", encoding="utf-8")
    (output / "source.json").write_text(json.dumps({"commit": sha, "tag": tag,
                                                 "component": profile["component"]},
                                                sort_keys=True) + "\n", encoding="utf-8")


def verify_output(output, sha, tag, profile=None):
    identity = json.loads((output / "source.json").read_text(encoding="utf-8"))
    if profile is not None and identity.get("component") != profile["component"]:
        raise ValueError("artifact component differs")
    if identity["commit"] != sha or identity["tag"] != tag:
        raise ValueError("artifact source identity differs")
    names = set()
    for line in (output / "checksums.txt").read_text(encoding="utf-8").splitlines():
        digest, name = line.split("  ", 1)
        if Path(name).name != name or not re.fullmatch(r"[a-f0-9]{64}", digest):
            raise ValueError("invalid checksum record")
        if hashlib.sha256((output / name).read_bytes()).hexdigest() != digest:
            raise ValueError("artifact checksum differs")
        names.add(name)
    if not names or {p.name for p in output.iterdir()} != names | {"checksums.txt", "source.json"}:
        raise ValueError("artifact set differs from checksums")
    if profile is not None:
        selected = "api" if tag.startswith("api/") else "runtime"
        expected = set()
        for artifact in profile["artifacts"]:
            if artifact["release"] != selected:
                continue
            target = artifact.get("target", {"os": "source", "arch": "any"})
            extension = ".zip" if target["os"] == "windows" else ".tar.gz"
            expected.add(artifact["name"] + "-" + tag.replace("/", "-") + "-"
                         + target["os"] + "-" + target["arch"] + extension)
        if not expected or names != expected:
            raise ValueError("artifact set differs from declared namespace targets")
    return names | {"checksums.txt", "source.json"}


def remote_tag(root, tag):
    lines = command(["git", "ls-remote", "origin", "refs/tags/" + tag,
                     "refs/tags/" + tag + "^{}"], cwd=root, capture=True).splitlines()
    refs = dict(line.split(None, 1)[::-1] for line in lines)
    return refs.get("refs/tags/" + tag + "^{}", refs.get("refs/tags/" + tag))


def publish(root, profile, output, tag, sha):
    if not TAG.fullmatch(tag) or not re.fullmatch(r"[a-f0-9]{40}", sha):
        raise ValueError("invalid release identity")
    if os.environ.get("GITHUB_EVENT_NAME") != "push" or os.environ.get("GITHUB_REF") != "refs/tags/" + tag:
        raise ValueError("publication requires the matching push tag event")
    head = clean_commit(root)
    output = validate_output_location(root, output)
    if head != sha or remote_tag(root, tag) != sha:
        raise ValueError("source/remote tag commit differs")
    names = verify_output(output, sha, tag, profile)
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    if not re.fullmatch(r"endless-net/[a-z][a-z0-9-]*", repository):
        raise ValueError("repository identity missing")
    # gh release view resolves both published tags and pending draft tags.
    view_args = ["gh", "release", "view", tag, "--repo", repository, "--json",
                 "tagName,targetCommitish,isDraft,isPrerelease,assets"]
    view = subprocess.run(view_args, cwd=root, capture_output=True, text=True)
    if view.returncode:
        if "release not found" not in view.stderr.lower():
            raise ValueError("release query failed: " + view.stderr)
        if remote_tag(root, tag) != sha:
            raise ValueError("remote tag moved")
        command(["gh", "release", "create", tag, "--repo", repository, "--verify-tag",
                 "--draft", "--target", sha, "--title", tag, "--generate-notes"], cwd=root)
        existing = json.loads(command(view_args, cwd=root, capture=True))
    else:
        existing = json.loads(view.stdout)
    if (existing.get("tagName") != tag or existing.get("targetCommitish") != sha
            or existing.get("isPrerelease") is not False):
        raise ValueError("existing release identity differs")
    actual = {asset["name"] for asset in existing["assets"]}
    if actual - names or (not existing["isDraft"] and actual != names):
        raise ValueError("existing published asset set differs")
    with tempfile.TemporaryDirectory() as temporary:
        for name in sorted(actual):
            command(["gh", "release", "download", tag, "--repo", repository,
                     "--pattern", name, "--dir", temporary], cwd=root)
            if (Path(temporary) / name).read_bytes() != (output / name).read_bytes():
                raise ValueError("existing asset bytes differ: " + name)
        for name in sorted(names - actual):
            command(["gh", "release", "upload", tag, str(output / name), "--repo", repository], cwd=root)
    final = json.loads(command(view_args, cwd=root, capture=True))
    if {asset["name"] for asset in final["assets"]} != names or remote_tag(root, tag) != sha:
        raise ValueError("release changed during upload")
    if (final.get("tagName") != tag or final.get("targetCommitish") != sha
            or final.get("isPrerelease") is not False):
        raise ValueError("release identity changed during upload")
    with tempfile.TemporaryDirectory() as verify_directory:
        for name in sorted(names):
            command(["gh", "release", "download", tag, "--repo", repository,
                     "--pattern", name, "--dir", verify_directory], cwd=root)
            if (Path(verify_directory) / name).read_bytes() != (output / name).read_bytes():
                raise ValueError("uploaded asset bytes differ: " + name)
    if final["isDraft"]:
        command(["gh", "release", "edit", tag, "--repo", repository, "--draft=false",
                 "--prerelease=false", "--latest=" + ("false" if tag.startswith("api/") else "true")], cwd=root)
    print("PUBLISHED", repository, tag, sha, flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=["check", "build", "publish"])
    parser.add_argument("--root", default=".")
    parser.add_argument("--profile", default=".ci/profile.json")
    parser.add_argument("--out", default=".artifacts")
    parser.add_argument("--tag")
    parser.add_argument("--sha")
    parser.add_argument("--lint-config")
    args = parser.parse_args()
    root = Path(args.root).resolve()
    profile = load_profile(root, args.profile)
    output = Path(args.out)
    if not output.is_absolute():
        output = root / output
    if args.operation == "check":
        check(root, profile, args.lint_config)
    elif args.operation == "build":
        build(root, profile, output, args.tag)
    else:
        if not args.tag or not args.sha:
            raise ValueError("tag/sha required")
        publish(root, profile, output, args.tag, args.sha)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as failure:
        print("CI FAILED:", failure, file=sys.stderr)
        sys.exit(1)
