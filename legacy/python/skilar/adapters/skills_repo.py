"""Skills repo adapter for clasing-skill.

Installs the skills package via setup.sh on Unix and a native Python path on Windows.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from datetime import datetime, timezone
from pathlib import Path
import sys

from ..installer import InstallResult, TargetResult
from ..models import InstallRequest, PackageDefinition
from ..opencode_config import merge_opencode_mcp_config


class SkillsRepoInstaller:
    """Installer for the skills repository.

    Delegates to the checked-out repo's scripts/setup.sh on Unix and a native
    Python flow on Windows, avoiding duplication of target-specific logic.
    """

    def install(
        self,
        checkout_dir: Path,
        request: InstallRequest,
        package: PackageDefinition,
    ) -> InstallResult:
        """Install skills package by running setup.sh for each target.

        Runs setup.sh --claude then --opencode sequentially so failures
        are attributable by target.

        SECURITY WARNING: This executes setup.sh from the checked-out repo.
        The script has full access to your system. Only install from trusted
        sources (e.g., official joeldevz/skills repository).

        Args:
            checkout_dir: Path to the checked-out skills repo
            request: Original install request
            package: Package definition from catalog

        Returns:
            InstallResult with artifact paths per target

        Raises:
            subprocess.CalledProcessError: If setup.sh fails
        """
        setup_script = checkout_dir / "scripts" / "setup.sh"
        windows_native = self._is_windows()

        # Security: Verify the script is from a trusted source
        # In MVP, we only warn - more strict verification would require
        # signature verification or checksum validation
        print(f"\n⚠️  Security Notice: About to execute {setup_script}")
        print(f"   Source: {package.repo_url}")
        print(f"   This script will have full system access.")
        print(f"   Only proceed if you trust this source.\n")

        if not windows_native and not setup_script.exists():
            raise FileNotFoundError(f"Setup script not found at {setup_script}")

        # Get commit from the checked-out repo
        commit = self._get_commit(checkout_dir)

        targets_result: dict[str, TargetResult] = {}
        timestamp = self._get_iso_timestamp()

        # Install for each target sequentially
        for target in request.targets:
            if target == "claude":
                if windows_native:
                    self._install_claude_windows(checkout_dir)
                else:
                    self._install_claude(setup_script)
                targets_result["claude"] = TargetResult(
                    status="installed",
                    installed_at=timestamp,
                    artifacts=[
                        "~/.claude",
                        "~/.claude/agents",
                        "~/.claude/skills",
                        "~/.claude/CLAUDE.md",
                    ],
                )
            elif target == "opencode":
                if windows_native:
                    self._install_opencode_windows(checkout_dir)
                else:
                    self._install_opencode(setup_script)
                targets_result["opencode"] = TargetResult(
                    status="installed",
                    installed_at=timestamp,
                    artifacts=["~/.config/opencode"],
                )

        # Get version info
        requested_version = request.versions.get(package.id, package.default_version)
        # resolved_version comes from the checked-out commit - we'll use commit short hash
        # as a fallback since we don't have the resolved version object here
        resolved_version = (
            requested_version  # The installer will update this with actual resolved
        )

        return InstallResult(
            package_id=package.id,
            requested_version=requested_version,
            resolved_version=resolved_version,
            resolved_ref="",  # Will be updated by caller with actual resolved ref
            commit=commit,
            dirty=False,  # Will be updated by caller for workspace mode
            targets=targets_result,
        )

    def _install_claude(self, setup_script: Path) -> None:
        """Run setup.sh --claude.

        Args:
            setup_script: Path to setup.sh in the checked-out repo

        Raises:
            subprocess.CalledProcessError: If setup.sh fails
        """
        subprocess.run(
            ["bash", str(setup_script), "--claude"],
            check=True,
            capture_output=True,
            text=True,
        )

    def _install_claude_windows(self, checkout_dir: Path) -> None:
        """Materialize the source assets without executing repository code."""
        source_dir = checkout_dir / "opencode" / "skills"
        claude_source = checkout_dir / "claude-code"
        target_dir = Path.home() / ".claude"
        if not source_dir.exists() or not claude_source.exists():
            raise FileNotFoundError("Claude source assets are missing")
        target_dir.mkdir(parents=True, exist_ok=True)
        target_skills = target_dir / "skills"
        if target_skills.exists():
            shutil.rmtree(target_skills)
        shutil.copytree(source_dir, target_skills)
        claude_md = claude_source / "CLAUDE.md"
        if claude_md.exists():
            shutil.copy2(claude_md, target_dir / "CLAUDE.md")

    def _install_opencode(self, setup_script: Path) -> None:
        """Run setup.sh --opencode.

        Args:
            setup_script: Path to setup.sh in the checked-out repo

        Raises:
            subprocess.CalledProcessError: If setup.sh fails
        """
        subprocess.run(
            ["bash", str(setup_script), "--opencode"],
            check=True,
            capture_output=True,
            text=True,
        )

    def _install_opencode_windows(self, checkout_dir: Path) -> None:
        """Install OpenCode natively on Windows."""
        source_dir = checkout_dir / "opencode"
        if not source_dir.exists():
            raise FileNotFoundError(
                f"OpenCode source directory not found at {source_dir}"
            )

        target_dir = Path.home() / ".config" / "opencode"
        backup_dir = self._backup_opencode_dir(target_dir)

        if target_dir.exists():
            shutil.rmtree(target_dir)

        shutil.copytree(source_dir, target_dir)

        if backup_dir is not None:
            self._merge_opencode_backup(target_dir, backup_dir)

    def _backup_opencode_dir(self, target_dir: Path) -> Path | None:
        """Create a timestamped backup of the current OpenCode config if needed."""
        if not target_dir.exists():
            return self._latest_opencode_backup(target_dir)

        backup_dir = (
            target_dir.parent
            / f"{target_dir.name}.backup.{self._get_backup_timestamp()}"
        )
        shutil.copytree(target_dir, backup_dir)
        return backup_dir

    def _latest_opencode_backup(self, target_dir: Path) -> Path | None:
        """Return the newest existing OpenCode backup directory, if any."""
        backups = sorted(
            target_dir.parent.glob(f"{target_dir.name}.backup.*"),
            key=lambda path: path.stat().st_mtime,
            reverse=True,
        )
        return backups[0] if backups else None

    def _merge_opencode_backup(self, target_dir: Path, backup_dir: Path) -> None:
        """Merge preserved MCP entries and restore Context7 credentials."""
        target_config = target_dir / "opencode.json"
        backup_config = backup_dir / "opencode.json"

        if not target_config.exists() or not backup_config.exists():
            return

        installed = json.loads(target_config.read_text(encoding="utf-8"))
        backup = json.loads(backup_config.read_text(encoding="utf-8"))
        merged = merge_opencode_mcp_config(installed, backup)

        context7_key = (
            backup.get("mcp", {})
            .get("context7", {})
            .get("headers", {})
            .get("CONTEXT7_API_KEY", "")
        )
        if context7_key and context7_key != "SET_IN_LOCAL_CONFIG":
            merged.setdefault("mcp", {}).setdefault("context7", {}).setdefault(
                "headers", {}
            )["CONTEXT7_API_KEY"] = context7_key
            merged["mcp"]["context7"]["enabled"] = True

        target_config.write_text(
            json.dumps(merged, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )

    def _is_windows(self) -> bool:
        """Return True when running on Windows."""
        return os.name == "nt" or sys.platform.startswith("win")

    def _get_commit(self, checkout_dir: Path) -> str:
        """Get the current commit SHA from the checked-out repo.

        Args:
            checkout_dir: Path to the checked-out repo

        Returns:
            Full commit SHA
        """
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=checkout_dir,
            capture_output=True,
            text=True,
            check=True,
        )
        return result.stdout.strip()

    def _get_iso_timestamp(self) -> str:
        """Get current timestamp in ISO format."""
        return datetime.now(timezone.utc).isoformat()

    def _get_backup_timestamp(self) -> str:
        """Get a filesystem-safe timestamp for backups."""
        return datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
