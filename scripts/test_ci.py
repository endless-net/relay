"""Unit checks for the autonomous public CI contract; no network or real releases."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("public_ci", Path(__file__).with_name("ci.py"))
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)


class PublicCI(unittest.TestCase):
    def committed_checkout(self, root):
        root.mkdir()
        (root / ".gitignore").write_text(".artifacts/\n", encoding="utf-8")
        (root / "main.go").write_text("package main\nfunc main() {}\n", encoding="utf-8")
        for args in (["init", "-q"], ["config", "core.autocrlf", "false"],
                     ["add", "."], ["-c", "user.name=Fixture", "-c",
                     "user.email=fixture@example.invalid", "commit", "-qm", "fixture"]):
            subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)
        return ci.command(["git", "rev-parse", "HEAD"], cwd=root, capture=True)

    def source_profile(self):
        return {"component": "fixture", "artifacts": [
            {"release": "runtime", "kind": "source", "name": "runtime", "path": "."},
            {"release": "api", "kind": "source", "name": "api", "path": "."}]}

    def test_build_rejects_dirty_tracked_and_untracked_inputs_before_packaging(self):
        for filename in ("main.go", "untracked.go"):
            for tag in (None, "v0.1.0", "api/v0.1.0"):
                with self.subTest(filename=filename, tag=tag), tempfile.TemporaryDirectory() as temp:
                    root = Path(temp) / "repo"
                    self.committed_checkout(root)
                    (root / filename).write_text("changed input", encoding="utf-8")
                    with patch.object(ci, "archive_bytes") as archive:
                        with self.assertRaisesRegex(ValueError, "clean"):
                            ci.build(root, self.source_profile(), root / ".artifacts", tag)
                        archive.assert_not_called()

    def test_build_rejects_source_mutation_and_commit_change_during_compilation(self):
        for mutation in ("tracked", "untracked", "commit"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temp:
                root = Path(temp) / "repo"
                self.committed_checkout(root)
                profile = {"component": "fixture", "artifacts": [{
                    "release": "runtime", "kind": "go", "name": "fixture", "module": ".",
                    "package": "./cmd/fixture", "target": {"os": "linux", "arch": "amd64"}}]}
                real_command = ci.command
                def compile_and_mutate(args, cwd=None, capture=False, env=None):
                    if args[:2] == ["go", "build"]:
                        Path(args[args.index("-o") + 1]).write_bytes(b"compiled fixture")
                        (root / ("untracked.go" if mutation == "untracked" else "main.go")).write_text(
                            "changed input", encoding="utf-8")
                        if mutation == "commit":
                            real_command(["git", "add", "."], cwd=root)
                            real_command(["git", "-c", "user.name=Fixture", "-c",
                                          "user.email=fixture@example.invalid", "commit", "-qm", "changed"], cwd=root)
                        return ""
                    return real_command(args, cwd=cwd, capture=capture, env=env)
                with patch.object(ci, "command", side_effect=compile_and_mutate):
                    with self.assertRaisesRegex(ValueError, "clean|commit changed"):
                        ci.build(root, profile, root / ".artifacts", "v0.1.0")
                self.assertFalse((root / ".artifacts/source.json").exists())

    def test_build_requires_committed_checkout_and_ignored_or_external_output(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            self.committed_checkout(root)
            with self.assertRaisesRegex(ValueError, "output.*ignored|ignored.*output"):
                ci.build(root, self.source_profile(), root / "unignored-output")
            ci.build(root, self.source_profile(), root / ".artifacts", "v0.1.0")
            ci.build(root, self.source_profile(), Path(temp) / "external", "api/v0.1.0")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q"], cwd=root, check=True, capture_output=True)
            with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                ci.build(root, self.source_profile(), root / ".artifacts")

    def test_dirty_publish_is_rejected_before_any_github_operation(self):
        for filename in ("main.go", "untracked.go"):
            with self.subTest(filename=filename), tempfile.TemporaryDirectory() as temp:
                root = Path(temp) / "repo"
                sha = self.committed_checkout(root)
                output = self.output(root)
                # The output is an ignored build directory, independent of source mutations.
                output.rename(root / ".artifacts")
                output = root / ".artifacts"
                identity = json.loads((output / "source.json").read_text())
                identity["commit"] = sha
                (output / "source.json").write_text(json.dumps(identity))
                (root / filename).write_text("changed input", encoding="utf-8")
                real_run = subprocess.run
                def forbid_github(args, *positional, **kwargs):
                    if args[0] == "gh":
                        self.fail("dirty source reached a GitHub operation")
                    return real_run(args, *positional, **kwargs)
                with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "push",
                        "GITHUB_REF": "refs/tags/v0.1.0", "GITHUB_REPOSITORY": "endless-net/fixture"}), patch.object(
                        ci, "remote_tag", return_value=sha), patch.object(ci.subprocess, "run", side_effect=forbid_github):
                    with self.assertRaisesRegex(ValueError, "clean"):
                        ci.publish(root, {"component": "fixture", "artifacts": [{"release": "runtime",
                            "kind": "source", "name": "fixture", "path": "."}]}, output, "v0.1.0", sha)

    def test_direct_source_archive_uses_committed_bytes_in_a_dirty_tree(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            self.committed_checkout(root)
            committed = (root / "main.go").read_bytes()
            (root / "main.go").write_bytes(b"dirty tracked input")
            (root / "untracked.go").write_bytes(b"untracked input")
            entries = ci.archive_bytes(root, {"path": "."})
            self.assertEqual(entries["main.go"][0], committed)
            self.assertNotIn("untracked.go", entries)

    def test_owned_dart_and_flutter_packages_cannot_be_omitted(self):
        for kind in ("dart", "flutter"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                (root / "api").mkdir()
                (root / "api/pubspec.yaml").write_text("name: fixture\n")
                data = {"component": "fixture", "modules": [{"path": "api", "kind": kind}],
                        "artifacts": [{"release": "runtime", "kind": "source", "name": "fixture", "path": "api"}]}
                profile = root / "profile.json"
                profile.write_text(json.dumps(data))
                ci.load_profile(root, "profile.json")
                (root / "undeclared").mkdir()
                (root / "undeclared/pubspec.yaml").write_text("name: undeclared\n")
                with self.assertRaisesRegex(ValueError, "every owned Dart/Flutter package"):
                    ci.load_profile(root, "profile.json")
                (root / "undeclared/pubspec.yaml").unlink()
                for cache in (".git", ".artifacts", ".dart_tool", "build", "vendor", ".venv"):
                    directory = root / cache / "cache-package"
                    directory.mkdir(parents=True)
                    (directory / "pubspec.yaml").write_text("name: cache\n")
                ci.load_profile(root, "profile.json")

    def test_tags_are_normal_v0_only(self):
        for value in ("v0.1.0", "api/v0.2.3"):
            self.assertIsNotNone(ci.TAG.fullmatch(value))
        for value in ("v1.0.0", "v0.01.0", "api/v0.1.0-rc.1", "v0.1", "../v0.1.0"):
            self.assertIsNone(ci.TAG.fullmatch(value))

    def test_declared_module_cannot_disappear(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "profile.json").write_text(json.dumps({
                "component": "fixture", "modules": [{"path": ".", "kind": "go"}],
                "artifacts": [{"release": "runtime", "kind": "source",
                               "name": "fixture", "path": "."}]}))
            with self.assertRaisesRegex(ValueError, "manifest is missing"):
                ci.load_profile(root, "profile.json")

    def test_paths_cannot_escape_checkout(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            with self.assertRaisesRegex(ValueError, "relative path|escapes"):
                ci.local(root, "..")

    def test_archive_bytes_reproducible(self):
        with tempfile.TemporaryDirectory() as temp:
            for suffix in (".tar.gz", ".zip"):
                first, second = Path(temp) / ("first" + suffix), Path(temp) / ("second" + suffix)
                entries = {"tool": (b"binary fixture", 0o755), "nested/source": (b"source", 0o644)}
                ci.write_archive(first, entries)
                ci.write_archive(second, dict(reversed(list(entries.items()))))
                self.assertEqual(first.read_bytes(), second.read_bytes())

    def output(self, root):
        output = root / "output"
        output.mkdir()
        archive = output / "fixture-v0.1.0-source-any.tar.gz"
        archive.write_bytes(b"fixture artifact")
        digest = ci.hashlib.sha256(archive.read_bytes()).hexdigest()
        (output / "checksums.txt").write_text(digest + "  fixture-v0.1.0-source-any.tar.gz\n")
        (output / "source.json").write_text(json.dumps({
            "commit": "a" * 40, "tag": "v0.1.0", "component": "fixture"}))
        return output

    def test_checksum_tamper_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            output = self.output(Path(temp))
            (output / "fixture-v0.1.0-source-any.tar.gz").write_bytes(b"tampered")
            with self.assertRaisesRegex(ValueError, "checksum differs"):
                ci.verify_output(output, "a" * 40, "v0.1.0")

    def test_api_and_runtime_are_distinct_subsets(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            self.committed_checkout(root)
            profile = {"component": "fixture", "artifacts": [
                {"release": "runtime", "kind": "source", "name": "runtime", "path": "."},
                {"release": "api", "kind": "source", "name": "api", "path": "."}]}
            ci.build(root, profile, root / ".artifacts", "api/v0.1.0")
            archives = [p.name for p in (root / ".artifacts").glob("*.tar.gz")]
            self.assertEqual(len(archives), 1)
            self.assertTrue(archives[0].startswith("api-"))

    def test_publish_rejects_non_tag_event(self):
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "push",
                                   "GITHUB_REF": "refs/heads/main"}, clear=True):
            with self.assertRaisesRegex(ValueError, "matching push tag"):
                ci.publish(Path("."), {}, Path("."), "v0.1.0", "a" * 40)

    def publication(self, conflict=False, draft=False, prerelease=False, corrupt_uploaded=False, missing=False):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            output = self.output(root)
            names = {"fixture-v0.1.0-source-any.tar.gz", "checksums.txt", "source.json"}
            existing = {"tagName": "v0.1.0", "targetCommitish": "a" * 40,
                        "isDraft": draft or missing, "isPrerelease": prerelease, "assets": [{"name": name} for name in sorted(
                            {"fixture-v0.1.0-source-any.tar.gz"} if draft else names)]}
            if missing:
                existing["assets"] = []
            uploaded, edits = [], []
            def fake_command(args, cwd=None, capture=False, env=None):
                if args[:3] == ["git", "rev-parse", "HEAD"]:
                    return "a" * 40
                if args[:3] == ["gh", "release", "view"]:
                    return json.dumps(existing)
                if args[:3] == ["gh", "release", "create"]:
                    self.assertIn("--draft", args)
                    self.assertIn("--verify-tag", args)
                    return ""
                if args[:3] == ["gh", "release", "download"]:
                    name = args[args.index("--pattern") + 1]
                    directory = Path(args[args.index("--dir") + 1])
                    (directory / name).write_bytes(b"conflict" if conflict or (corrupt_uploaded and name in uploaded) else (output / name).read_bytes())
                elif args[:3] == ["gh", "release", "upload"]:
                    name = Path(args[4]).name
                    uploaded.append(name)
                    existing["assets"].append({"name": name})
                elif args[:3] == ["gh", "release", "edit"]:
                    edits.append(args)
                    existing["isDraft"] = False
                else:
                    self.fail("unexpected command " + repr(args))
                return ""
            with patch.dict(os.environ, {
                "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/tags/v0.1.0",
                "GITHUB_REPOSITORY": "endless-net/fixture"}, clear=True), patch.object(
                    ci, "remote_tag", return_value="a" * 40), patch.object(
                    ci, "clean_commit", return_value="a" * 40), patch.object(
                    ci, "validate_output_location", return_value=output), patch.object(
                    ci, "command", side_effect=fake_command), patch.object(
                    ci.subprocess, "run", return_value=SimpleNamespace(
                        returncode=1 if missing else 0, stdout=json.dumps(existing),
                        stderr="release not found" if missing else "")):
                profile = {"component": "fixture", "artifacts": [
                    {"release": "runtime", "kind": "source", "name": "fixture", "path": "."}]}
                ci.publish(root, profile, output, "v0.1.0", "a" * 40)
            return uploaded, edits

    def test_profile_rejects_incomplete_namespace(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = self.output(Path(temporary))
            profile = {"component": "fixture", "artifacts": [
                {"release": "api", "kind": "source", "name": "api", "path": "."}]}
            with self.assertRaisesRegex(ValueError, "declared namespace"):
                ci.verify_output(output, "a" * 40, "v0.1.0", profile)

    def test_artifact_component_must_match_profile(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = self.output(Path(temporary))
            with self.assertRaisesRegex(ValueError, "component differs"):
                ci.verify_output(output, "a" * 40, "v0.1.0", {"component": "other"})

    def test_prerelease_cannot_be_an_identical_release(self):
        with self.assertRaisesRegex(ValueError, "identity differs"):
            self.publication(prerelease=True)

    def test_new_upload_bytes_are_verified(self):
        with self.assertRaisesRegex(ValueError, "uploaded asset bytes differ"):
            self.publication(draft=True, corrupt_uploaded=True)

    def test_block_style_first_party_replace_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "go.mod").write_text("module fixture\nreplace (\n github.com/endless-net/identity/api => ../other\n)\n")
            parsed = {"Replace": [{"Old": {"Path": "github.com/endless-net/identity/api"},
                                    "New": {"Path": "../other"}}]}
            with patch.object(ci, "command", return_value=json.dumps(parsed)):
                with self.assertRaisesRegex(ValueError, "replace is forbidden"):
                    ci.check(root, {"modules": [{"path": ".", "kind": "go"}]})

    def test_identical_published_retry_does_not_mutate(self):
        uploaded, edits = self.publication()
        self.assertEqual(uploaded, [])
        self.assertEqual(edits, [])

    def test_existing_asset_conflict_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "asset bytes differ"):
            self.publication(conflict=True)

    def test_interrupted_draft_only_uploads_missing(self):
        uploaded, edits = self.publication(draft=True)
        self.assertEqual(set(uploaded), {"checksums.txt", "source.json"})
        self.assertEqual(len(edits), 1)

    def test_absent_release_is_created_and_reloaded_through_draft_aware_cli(self):
        uploaded, edits = self.publication(missing=True)
        self.assertEqual(len(uploaded), 3)
        self.assertEqual(len(edits), 1)

    def test_duplicate_json_keys_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "duplicate.json"
            path.write_text('{"valid": true, "valid": false}')
            with self.assertRaisesRegex(ValueError, "duplicate JSON key"):
                ci.load_json(path)


if __name__ == "__main__":
    unittest.main()
