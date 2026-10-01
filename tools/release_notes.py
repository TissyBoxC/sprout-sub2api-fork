#!/usr/bin/env python3
"""Generate Chinese release notes for the sprout Sub2API fork."""

from __future__ import annotations

import argparse
import datetime as dt
import os
import re
import subprocess
from pathlib import Path

SECTION_ORDER = (
    "feat",
    "fix",
    "perf",
    "refactor",
    "security",
    "build",
    "ci",
    "test",
    "docs",
    "chore",
    "other",
)

SECTION_TITLES = {
    "feat": "新功能",
    "fix": "问题修复",
    "perf": "性能优化",
    "refactor": "代码重构",
    "security": "安全与隐私",
    "build": "构建与依赖",
    "ci": "持续集成",
    "test": "测试",
    "docs": "文档",
    "chore": "维护",
    "other": "其他变更",
}

SCOPE_TITLES = {
    "api": "接口与协议",
    "billing": "计费与配额",
    "config": "配置",
    "fork": "芽系列分支",
    "frontend": "管理界面",
    "gateway": "AI 网关",
    "release": "版本发布",
    "repo": "仓库维护",
    "sprout": "芽系列扩展",
    "sub2api": "AI 网关",
}

DESCRIPTION_TITLES = {
    "add": "新增",
    "align": "对齐",
    "define": "定义",
    "enforce": "强制校验",
    "expose": "开放",
    "keep": "保留",
    "rename": "重命名",
    "repair": "修复",
    "reserve": "预留",
    "share": "共享",
    "sync": "同步",
    "update": "更新",
    "validate": "校验",
    "verify": "验证",
}

DESCRIPTION_PHRASES = {
    "internal api config guard": "新增内部接口配置校验",
    "internal api contract": "新增内部接口契约",
    "request label middleware": "新增请求标签中间件",
    "secured internal runtime api": "开放受保护的内部运行时接口",
}

CONVENTIONAL_COMMIT = re.compile(
    r"^(?P<type>[A-Za-z]+)"
    r"(?:\((?P<scope>[^)]+)\))?"
    r"(?P<breaking>!)?: "
    r"(?P<description>.+)$"
)
RELEASE_TAG = re.compile(r"^v(?P<version>\d+\.\d+\.\d+)$")
MAX_COMMITS_PER_SECTION = 50


def run_git(*arguments: str) -> str:
    result = subprocess.run(
        ["git", *arguments],
        check=True,
        capture_output=True,
        text=True,
        encoding="utf-8",
    )
    return result.stdout.strip()


def resolve_previous_tag(current_ref: str, current_tag: str) -> str | None:
    tags = run_git(
        "tag",
        "--merged",
        current_ref,
        "--sort=-v:refname",
        "--format=%(refname:short)",
    ).splitlines()
    for tag in tags:
        if RELEASE_TAG.fullmatch(tag) and tag != current_tag:
            return tag
    return None


def read_commits(previous_tag: str | None, current_ref: str) -> list[dict[str, str]]:
    revision_range = f"{previous_tag}..{current_ref}" if previous_tag else current_ref
    output = run_git(
        "log",
        "--no-merges",
        "--format=%H%n%an%n%s%n%b%n%x1e",
        revision_range,
    )

    commits: list[dict[str, str]] = []
    for raw_record in output.split("\x1e"):
        fields = raw_record.strip().splitlines()
        if len(fields) < 3:
            continue
        sha, author, subject = fields[:3]
        body = "\n".join(fields[3:])
        match = CONVENTIONAL_COMMIT.match(subject)
        if match:
            commit_type = match.group("type").lower()
            scope = match.group("scope") or ""
            description = match.group("description")
            is_breaking = bool(match.group("breaking")) or "BREAKING CHANGE:" in body
        else:
            commit_type = "other"
            scope = ""
            description = subject
            is_breaking = "BREAKING CHANGE:" in body
        if commit_type not in SECTION_TITLES:
            commit_type = "other"
        commits.append(
            {
                "sha": sha,
                "author": author,
                "scope": scope,
                "type": commit_type,
                "description": description,
                "is_breaking": str(is_breaking).lower(),
            }
        )
    return commits


def localize_description(description: str) -> str:
    phrase = DESCRIPTION_PHRASES.get(description.lower())
    if phrase is not None:
        return phrase

    words = description.split(maxsplit=1)
    if not words:
        return description
    action = DESCRIPTION_TITLES.get(words[0].lower())
    if action is None:
        return "完善相关功能"
    return action


def render_commit_lines(commits: list[dict[str, str]], repository: str) -> list[str]:
    lines: list[str] = []
    for commit in commits:
        scope = SCOPE_TITLES.get(commit["scope"], commit["scope"] or "AI 网关")
        short_sha = commit["sha"][:7]
        if repository:
            reference = (
                f"[{short_sha}](https://github.com/{repository}/commit/{commit['sha']})"
            )
        else:
            reference = f"`{short_sha}`"
        lines.append(
            f"- **{scope}**：{localize_description(commit['description'])}"
            f"（提交 {reference}，作者：{commit['author']}）"
        )
    return lines


def render_release_notes(
    version: str,
    repository: str,
    previous_tag: str | None,
    commits: list[dict[str, str]],
) -> str:
    tag = f"v{version}"
    comparison = (
        f"`{previous_tag}` 至 `{tag}`"
        if previous_tag
        else f"首个发行版本 `{tag}`"
    )
    lines = [
        f"# 初芽 AI 网关 {tag}",
        "",
        f"发布日期：{dt.date.today().isoformat()}",
        "",
        f"比较范围：{comparison}",
        "",
        "本版本相对上一发行版本的改动如下。",
        "",
        "## 版本摘要",
        "",
    ]

    counts = {section: 0 for section in SECTION_ORDER}
    for commit in commits:
        counts[commit["type"]] += 1
    for section in SECTION_ORDER:
        if counts[section]:
            lines.append(f"- {SECTION_TITLES[section]}：{counts[section]} 项")
    if not commits:
        lines.append("- 本版本没有代码提交。")

    breaking_changes = [commit for commit in commits if commit["is_breaking"] == "true"]
    if breaking_changes:
        lines.extend(["", "## 破坏性变更", ""])
        lines.extend(render_commit_lines(breaking_changes, repository))

    for section in SECTION_ORDER:
        selected = [commit for commit in commits if commit["type"] == section]
        if selected:
            lines.extend(["", f"## {SECTION_TITLES[section]}", ""])
            lines.extend(render_commit_lines(selected[:MAX_COMMITS_PER_SECTION], repository))
            if len(selected) > MAX_COMMITS_PER_SECTION:
                lines.append(
                    f"- 本分类还有 {len(selected) - MAX_COMMITS_PER_SECTION} 项改动，"
                    "完整记录请查看提交历史。"
                )
    return "\n".join(lines).rstrip() + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", required=True)
    parser.add_argument("--current-ref", default="HEAD")
    parser.add_argument("--current-tag", required=True)
    parser.add_argument("--previous-tag")
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", ""))
    parser.add_argument("--output", default="release-notes.md")
    arguments = parser.parse_args()

    if not re.fullmatch(r"\d+\.\d+\.\d+", arguments.version):
        raise SystemExit(f"invalid release version: {arguments.version}")
    previous_tag = arguments.previous_tag
    if previous_tag is None:
        previous_tag = resolve_previous_tag(arguments.current_ref, arguments.current_tag)
    notes = render_release_notes(
        arguments.version,
        arguments.repository,
        previous_tag,
        read_commits(previous_tag, arguments.current_ref),
    )
    Path(arguments.output).write_text(notes, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
