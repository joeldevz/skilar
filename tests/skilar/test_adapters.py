"""Tests for clasing-skill package adapters."""

from __future__ import annotations

import json
import tempfile
import sys
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

# Ensure scripts is importable
sys.path.insert(0, str(Path(__file__).parent.parent.parent))

from scripts.clasing_skill.adapters.skills_repo import SkillsRepoInstaller
from scripts.clasing_skill.adapters.neurox import NeuroxInstaller
from scripts.clasing_skill.installer import (
    InstallResult,
    TargetResult,
    install_packages,
    InstallError,
)
from scripts.clasing_skill.models import InstallRequest, PackageDefinition


class TestSkillsRepoInstaller(unittest.TestCase):
    """Tests for SkillsRepoInstaller."""

    def setUp(self) -> None:
        """Set up test fixtures."""
        self.installer = SkillsRepoInstaller()
        self.package = PackageDefinition(
            id="skills",
            display_name="Skills",
            repo_url="https://github.com/joeldevz/skills.git",
            adapter="skills_repo",
            supported_targets=("claude", "opencode"),
            default_version="latest",
            requires_neurox=True,
            install_strategy="git_checkout_setup_script",
        )

    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_install_claude_only(
        self, mock_exists: MagicMock, mock_run: MagicMock
    ) -> None:
        """Install for claude target only."""
        mock_run.return_value = MagicMock(stdout="abc123\n", stderr="", returncode=0)
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude"],
            versions={"skills": "latest"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/skills-abc123")

        with patch.object(self.installer, "_get_commit", return_value="abc123def456"):
            with patch.object(
                self.installer,
                "_get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                result = self.installer.install(checkout_dir, request, self.package)

        self.assertEqual(result.package_id, "skills")
        self.assertEqual(result.commit, "abc123def456")
        self.assertIn("claude", result.targets)
        self.assertNotIn("opencode", result.targets)

        claude_result = result.targets["claude"]
        self.assertEqual(claude_result.status, "installed")
        self.assertIn("~/.claude", claude_result.artifacts)
        self.assertIn("~/.claude/agents", claude_result.artifacts)
        self.assertIn("~/.claude/skills", claude_result.artifacts)
        self.assertIn("~/.claude/CLAUDE.md", claude_result.artifacts)

    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_install_opencode_only(
        self, mock_exists: MagicMock, mock_run: MagicMock
    ) -> None:
        """Install for opencode target only."""
        mock_run.return_value = MagicMock(stdout="abc123\n", stderr="", returncode=0)
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["opencode"],
            versions={"skills": "latest"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/skills-abc123")

        with patch.object(self.installer, "_get_commit", return_value="abc123def456"):
            with patch.object(
                self.installer,
                "_get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                result = self.installer.install(checkout_dir, request, self.package)

        self.assertEqual(result.package_id, "skills")
        self.assertIn("opencode", result.targets)
        self.assertNotIn("claude", result.targets)

        opencode_result = result.targets["opencode"]
        self.assertEqual(opencode_result.status, "installed")
        self.assertEqual(opencode_result.artifacts, ["~/.config/opencode"])

    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_install_both_targets(
        self, mock_exists: MagicMock, mock_run: MagicMock
    ) -> None:
        """Install for both claude and opencode targets."""
        mock_run.return_value = MagicMock(stdout="abc123\n", stderr="", returncode=0)
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude", "opencode"],
            versions={"skills": "latest"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/skills-abc123")

        with patch.object(self.installer, "_get_commit", return_value="abc123def456"):
            with patch.object(
                self.installer,
                "_get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                result = self.installer.install(checkout_dir, request, self.package)

        self.assertIn("claude", result.targets)
        self.assertIn("opencode", result.targets)

    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_setup_script_called_with_correct_args(
        self, mock_exists: MagicMock, mock_run: MagicMock
    ) -> None:
        """Verify setup.sh is called with correct target arguments."""
        mock_run.return_value = MagicMock(stdout="abc123\n", stderr="", returncode=0)
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude"],
            versions={"skills": "latest"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/skills-abc123")
        setup_script = checkout_dir / "scripts" / "setup.sh"

        with patch.object(self.installer, "_get_commit", return_value="abc123def456"):
            with patch.object(
                self.installer,
                "_get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                self.installer.install(checkout_dir, request, self.package)

        # Check that setup.sh was called with --claude
        calls = mock_run.call_args_list
        setup_calls = [c for c in calls if "setup.sh" in str(c)]
        self.assertEqual(len(setup_calls), 1)
        self.assertIn("--claude", str(setup_calls[0]))

    def test_windows_install_uses_native_python_and_preserves_opencode_mcp(
        self,
    ) -> None:
        """Windows install should use native Python and preserve OpenCode MCP."""
        with tempfile.TemporaryDirectory() as temp_dir:
            temp_root = Path(temp_dir)
            checkout_dir = temp_root / "checkout"
            checkout_dir.mkdir()
            (checkout_dir / "claude-code").mkdir()
            (checkout_dir / "claude-code" / "CLAUDE.md").write_text("# Claude\n", encoding="utf-8")
            (checkout_dir / "opencode" / "skills").mkdir(parents=True)
            opencode_source = checkout_dir / "opencode"
            opencode_source.mkdir()
            (opencode_source / "opencode.json").write_text(
                json.dumps(
                    {
                        "mcp": {
                            "context7": {
                                "enabled": True,
                                "type": "remote",
                                "url": "https://mcp.context7.com/mcp",
                            },
                            "neurox": {
                                "command": ["neurox", "mcp"],
                                "enabled": True,
                                "type": "local",
                            },
                        }
                    }
                ),
                encoding="utf-8",
            )

            home_dir = temp_root / "home"
            target_dir = home_dir / ".config" / "opencode"
            target_dir.mkdir(parents=True)
            (target_dir / "opencode.json").write_text(
                json.dumps(
                    {
                        "mcp": {
                            "custom": {
                                "command": ["custom", "serve"],
                                "enabled": True,
                                "type": "local",
                            },
                            "context7": {
                                "enabled": True,
                                "type": "remote",
                                "headers": {"CONTEXT7_API_KEY": "secret-key"},
                                "url": "https://mcp.context7.com/mcp",
                            },
                        }
                    }
                ),
                encoding="utf-8",
            )

            request = InstallRequest(
                packages=["skills"],
                targets=["claude", "opencode"],
                versions={"skills": "latest"},
                interactive=False,
            )

            with patch.object(self.installer, "_is_windows", return_value=True):
                with patch.object(Path, "home", return_value=home_dir):
                    with patch.object(
                        self.installer, "_get_commit", return_value="abc123def456"
                    ):
                        with patch.object(
                            self.installer,
                            "_get_iso_timestamp",
                            return_value="2026-04-08T12:00:00+00:00",
                        ):
                            with patch.object(
                                self.installer,
                                "_get_backup_timestamp",
                                return_value="20260408-120000",
                            ):
                                with patch(
                                    "scripts.clasing_skill.adapters.skills_repo.subprocess.run"
                                ) as mock_run:
                                    mock_run.return_value = MagicMock(
                                        stdout="abc123\n", stderr="", returncode=0
                                    )
                                    result = self.installer.install(
                                        checkout_dir, request, self.package
                                    )

            self.assertEqual(
                result.targets["claude"].artifacts,
                [
                    "~/.claude",
                    "~/.claude/agents",
                    "~/.claude/skills",
                    "~/.claude/CLAUDE.md",
                ],
            )
            self.assertEqual(
                result.targets["opencode"].artifacts, ["~/.config/opencode"]
            )

            self.assertEqual(
                mock_run.call_args_list,
                [],
                "Windows native installation must not spawn legacy Python or shell installers",
            )

            merged_config = json.loads(
                (home_dir / ".config" / "opencode" / "opencode.json").read_text(
                    encoding="utf-8"
                )
            )
            self.assertIn("custom", merged_config["mcp"])
            self.assertEqual(
                merged_config["mcp"]["neurox"],
                {
                    "command": ["neurox", "mcp"],
                    "enabled": True,
                    "type": "local",
                },
            )
            self.assertEqual(
                merged_config["mcp"]["context7"]["headers"]["CONTEXT7_API_KEY"],
                "secret-key",
            )

    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_subprocess_failure_raises(
        self, mock_exists: MagicMock, mock_run: MagicMock
    ) -> None:
        """Subprocess failure should raise CalledProcessError."""
        from subprocess import CalledProcessError

        mock_run.side_effect = CalledProcessError(1, "bash", stderr="setup failed")
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude"],
            versions={"skills": "latest"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/skills-abc123")

        with self.assertRaises(CalledProcessError):
            self.installer.install(checkout_dir, request, self.package)


class TestNeuroxInstaller(unittest.TestCase):
    """Tests for NeuroxInstaller."""

    def setUp(self) -> None:
        """Set up test fixtures."""
        self.installer = NeuroxInstaller()
        self.package = PackageDefinition(
            id="neurox",
            display_name="Neurox",
            repo_url="https://github.com/joeldevz/neurox.git",
            adapter="neurox_binary",
            supported_targets=("claude", "opencode"),
            default_version="latest",
            requires_neurox=False,
            install_strategy="git_checkout_go_build",
        )

    @patch("scripts.clasing_skill.adapters.neurox.Path.mkdir")
    @patch("scripts.clasing_skill.adapters.neurox.shutil.copy2")
    @patch("scripts.clasing_skill.adapters.neurox.subprocess.run")
    def test_install_builds_and_installs(
        self, mock_run: MagicMock, mock_copy: MagicMock, mock_mkdir: MagicMock
    ) -> None:
        """Install builds neurox and copies to ~/.local/bin."""
        mock_run.return_value = MagicMock(stdout="abc123\n", stderr="", returncode=0)

        request = InstallRequest(
            packages=["neurox"],
            targets=["claude"],
            versions={"neurox": "v0.9.0"},
            interactive=False,
        )

        checkout_dir = Path("/tmp/neurox-abc123")

        with patch.object(self.installer, "_get_commit", return_value="def789"):
            with patch.object(
                self.installer,
                "_get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                result = self.installer.install(checkout_dir, request, self.package)

        self.assertEqual(result.package_id, "neurox")
        self.assertEqual(result.commit, "def789")
        self.assertIn("claude", result.targets)

        # Verify binary path is recorded as artifact
        claude_result = result.targets["claude"]
        self.assertEqual(len(claude_result.artifacts), 1)
        self.assertIn("neurox", claude_result.artifacts[0])

    @patch("scripts.clasing_skill.adapters.neurox.subprocess.run")
    def test_build_uses_fts5_tags(self, mock_run: MagicMock) -> None:
        """Build command includes -tags fts5."""
        mock_run.return_value = MagicMock(stdout="", stderr="", returncode=0)

        checkout_dir = Path("/tmp/neurox-abc123")

        self.installer._build_neurox(checkout_dir, "neurox")

        build_calls = [c for c in mock_run.call_args_list if "build" in str(c)]
        self.assertEqual(len(build_calls), 1)
        args = build_calls[0][0][0]  # First positional arg (the command list)
        self.assertIn("-tags", args)
        self.assertIn("fts5", args)

    def test_binary_filename_uses_windows_suffix(self) -> None:
        """Windows builds should use an .exe binary name."""
        with patch("scripts.clasing_skill.adapters.neurox.sys.platform", "win32"):
            self.assertEqual(self.installer._binary_filename(), "neurox.exe")

    @patch("scripts.clasing_skill.adapters.neurox.subprocess.run")
    def test_verify_runs_status(self, mock_run: MagicMock) -> None:
        """Verification runs 'neurox status'."""
        mock_run.return_value = MagicMock(stdout="", stderr="", returncode=0)

        install_path = Path.home() / ".local" / "bin" / "neurox"

        self.installer._verify_neurox(install_path)

        verify_calls = [c for c in mock_run.call_args_list if "status" in str(c)]
        self.assertEqual(len(verify_calls), 1)

    @patch("scripts.clasing_skill.adapters.neurox.subprocess.run")
    def test_build_failure_raises(self, mock_run: MagicMock) -> None:
        """Build failure should raise CalledProcessError."""
        from subprocess import CalledProcessError

        mock_run.side_effect = CalledProcessError(1, "go", stderr="build failed")

        checkout_dir = Path("/tmp/neurox-abc123")

        with self.assertRaises(CalledProcessError):
            self.installer._build_neurox(checkout_dir, "neurox")


class TestInstallPackages(unittest.TestCase):
    """Tests for install_packages function."""

    def setUp(self) -> None:
        """Set up test fixtures."""
        self.catalog = {
            "skills": PackageDefinition(
                id="skills",
                display_name="Skills",
                repo_url="https://github.com/joeldevz/skills.git",
                adapter="skills_repo",
                supported_targets=("claude", "opencode"),
                default_version="latest",
                requires_neurox=True,
                install_strategy="git_checkout_setup_script",
            ),
            "neurox": PackageDefinition(
                id="neurox",
                display_name="Neurox",
                repo_url="https://github.com/joeldevz/neurox.git",
                adapter="neurox_binary",
                supported_targets=("claude", "opencode"),
                default_version="latest",
                requires_neurox=False,
                install_strategy="git_checkout_go_build",
            ),
        }

    @patch("scripts.clasing_skill.installer.resolve_version")
    @patch("scripts.clasing_skill.installer.checkout_package")
    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_install_single_package(
        self,
        mock_exists: MagicMock,
        mock_run: MagicMock,
        mock_checkout: MagicMock,
        mock_resolve: MagicMock,
    ) -> None:
        """Install a single package successfully."""
        from scripts.clasing_skill.resolver import ResolvedVersion

        mock_resolve.return_value = ResolvedVersion(
            requested_selector="latest",
            resolved_version="v1.0.0",
            resolved_ref="refs/tags/v1.0.0",
            commit="abc123def456",
            repo_url="https://github.com/joeldevz/skills.git",
        )
        mock_checkout.return_value = Path("/tmp/skills-abc123")
        mock_run.return_value = MagicMock(stdout="", stderr="", returncode=0)
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude"],
            versions={"skills": "latest"},
            interactive=False,
        )

        with patch(
            "scripts.clasing_skill.adapters.skills_repo.SkillsRepoInstaller._get_commit",
            return_value="abc123def456",
        ):
            with patch(
                "scripts.clasing_skill.adapters.skills_repo.SkillsRepoInstaller._get_iso_timestamp",
                return_value="2026-04-08T12:00:00+00:00",
            ):
                results = install_packages(
                    request, self.catalog, temp_root=Path("/tmp")
                )

        self.assertEqual(len(results), 1)
        self.assertEqual(results[0].package_id, "skills")
        self.assertEqual(results[0].commit, "abc123def456")

    @patch("scripts.clasing_skill.installer.resolve_version")
    def test_unknown_adapter_raises(self, mock_resolve: MagicMock) -> None:
        """Unknown adapter should raise InstallError."""
        from scripts.clasing_skill.resolver import ResolvedVersion

        bad_catalog = {
            "badpkg": PackageDefinition(
                id="badpkg",
                display_name="Bad Package",
                repo_url="https://example.com/bad.git",
                adapter="unknown_adapter",
                supported_targets=("claude",),
                default_version="latest",
                requires_neurox=False,
                install_strategy="unknown",
            ),
        }

        mock_resolve.return_value = ResolvedVersion(
            requested_selector="latest",
            resolved_version="v1.0.0",
            resolved_ref="refs/tags/v1.0.0",
            commit="abc123",
            repo_url="https://example.com/bad.git",
        )

        request = InstallRequest(
            packages=["badpkg"],
            targets=["claude"],
            versions={"badpkg": "latest"},
            interactive=False,
        )

        with self.assertRaises(InstallError) as ctx:
            with patch(
                "scripts.clasing_skill.installer.checkout_package",
                return_value=Path("/tmp/bad-abc123"),
            ):
                install_packages(request, bad_catalog, temp_root=Path("/tmp"))

        self.assertIn("unknown_adapter", str(ctx.exception))

    @patch("scripts.clasing_skill.installer.resolve_version")
    def test_package_not_in_catalog_raises(self, mock_resolve: MagicMock) -> None:
        """Package not in catalog should raise InstallError."""
        request = InstallRequest(
            packages=["nonexistent"],
            targets=["claude"],
            versions={"nonexistent": "latest"},
            interactive=False,
        )

        with self.assertRaises(InstallError) as ctx:
            install_packages(request, self.catalog, temp_root=Path("/tmp"))

        self.assertIn("nonexistent", str(ctx.exception))

    @patch("scripts.clasing_skill.installer.resolve_version")
    @patch("scripts.clasing_skill.installer.checkout_package")
    @patch("scripts.clasing_skill.adapters.skills_repo.subprocess.run")
    @patch.object(Path, "exists")
    def test_first_failure_aborts_others(
        self,
        mock_exists: MagicMock,
        mock_run: MagicMock,
        mock_checkout: MagicMock,
        mock_resolve: MagicMock,
    ) -> None:
        """First subprocess failure should abort all subsequent installs."""
        from subprocess import CalledProcessError
        from scripts.clasing_skill.resolver import ResolvedVersion

        mock_resolve.return_value = ResolvedVersion(
            requested_selector="latest",
            resolved_version="v1.0.0",
            resolved_ref="refs/tags/v1.0.0",
            commit="abc123",
            repo_url="https://github.com/joeldevz/skills.git",
        )
        mock_checkout.return_value = Path("/tmp/skills-abc123")
        mock_run.side_effect = CalledProcessError(1, "bash", stderr="setup failed")
        mock_exists.return_value = True

        request = InstallRequest(
            packages=["skills"],
            targets=["claude"],
            versions={"skills": "latest"},
            interactive=False,
        )

        with self.assertRaises(InstallError) as ctx:
            install_packages(request, self.catalog, temp_root=Path("/tmp"))

        # The error should indicate installation failed
        self.assertIn("Installation failed", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
