#!/usr/bin/env python3
"""Generate Chinese release notes for the sprout Sub2API fork.

The fork keeps upstream history, so release notes must not treat every upstream
commit as a product change. The workflow passes an explicit fork baseline and,
when available, the previous release tag.
"""

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
    "automate chinese version releases": "自动化中文版本发布",
    "bounded chinese release notes": "限制中文发行说明的长度",
    "bump version to 0.2.13": "将版本号更新到 0.2.13",
    "generate bounded chinese release notes": "生成长度受限的中文发行说明",
    "generate fork-scoped chinese release notes": "生成仅覆盖芽系列改动的中文发行说明",
    "identify sprout sub2api fork": "标明芽系列 fork 品牌信息",
    "internal api config guard": "新增内部接口配置校验",
    "internal api contract": "新增内部接口契约",
    "localize release note fallback": "完善发行说明的中文回退文本",
    "localize fallback summary": "补充发行说明的中文摘要",
    "make asset publishing idempotent": "使发行附件发布可重复执行",
    "polish japanese fork notice": "完善日文 fork 说明",
    "preserve concrete release note descriptions": "保留具体的发行说明内容",
    "request label middleware": "新增请求标签中间件",
    "require explicit previous release range": "强制指定上一发行版本范围",
    "restrict tracked documentation": "仅跟踪代码与 README",
    "resolve fork baseline without upstream tags": "在不依赖上游标签的情况下解析分支基线",
    "select previous release tag explicitly": "明确选择上一发行标签",
    "secured internal runtime api": "开放受保护的内部运行时接口",
    "update axios to patched release": "更新 Axios 到安全修复版本",
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


def read_commits(
    previous_tag: str | None,
    fork_baseline: str | None,
    current_ref: str,
) -> list[dict[str, str]]:
    if previous_tag:
        revision_range = f"{previous_tag}..{current_ref}"
    elif fork_baseline:
        revision_range = f"{fork_baseline}..{current_ref}"
    else:
        revision_range = current_ref
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

    # Preserve the original wording when a commit is already readable in
    # Chinese, so release notes stay specific instead of becoming generic.
    if contains_cjk(description):
        return description.rstrip("。.!！")

    action = DESCRIPTION_TITLES.get(words[0].lower())
    if action is None:
        return description
    return action


def contains_cjk(text: str) -> bool:
    return any("\u4e00" <= character <= "\u9fff" for character in text)


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
    fork_baseline: str | None,
    fork_baseline_label: str | None,
    commits: list[dict[str, str]],
) -> str:
    tag = f"v{version}"
    if previous_tag:
        comparison = f"`{previous_tag}` 至 `{tag}`"
    elif fork_baseline:
        display_baseline = fork_baseline_label or fork_baseline
        comparison = f"上游基线 `{display_baseline}` 之后的芽系列改动，并在 `{tag}` 首次发行"
    else:
        comparison = f"首个发行版本 `{tag}`"
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
    parser.add_argument("--fork-baseline")
    parser.add_argument("--fork-baseline-label")
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", ""))
    parser.add_argument("--output", default="release-notes.md")
    arguments = parser.parse_args()

    if not re.fullmatch(r"\d+\.\d+\.\d+", arguments.version):
        raise SystemExit(f"invalid release version: {arguments.version}")
    scope_arguments = (
        bool(arguments.previous_tag),
        bool(arguments.fork_baseline),
    )
    if sum(scope_arguments) != 1:
        raise SystemExit(
            "exactly one of --previous-tag or --fork-baseline is required"
        )
    previous_tag = arguments.previous_tag
    notes = render_release_notes(
        arguments.version,
        arguments.repository,
        previous_tag,
        arguments.fork_baseline,
        arguments.fork_baseline_label,
        read_commits(previous_tag, arguments.fork_baseline, arguments.current_ref),
    )
    Path(arguments.output).write_text(notes, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
