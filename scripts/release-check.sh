#!/usr/bin/env bash
# 发版就绪度核查：make release-check VERSION=x.y.z
#
# 只做静态检查，不跑测试 —— 测试由 release.yml 的门禁（复用 ci.yml）负责，
# 这里挡的是「流程上漏了一步」：分支不对、tag 被占、CHANGELOG 没改名、
# 加了迁移却只升 PATCH……规范见 docs/11-branching-release.md。
#
# 只读：不改文件、不打 tag、不推送。
set -euo pipefail

version="${1:-}"
fail=0
err()  { echo "✗ $*" >&2; fail=1; }
ok()   { echo "✓ $*"; }
warn() { echo "! $*"; }

if [ -z "$version" ]; then
  echo "用法：make release-check VERSION=x.y.z" >&2
  exit 2
fi

# 1. 版本号格式：正式版只认 X.Y.Z（RC tag 在 release 分支上另打，不走这里）
if ! printf '%s' "$version" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "✗ $version 不是 X.Y.Z 形式的正式版本号" >&2
  exit 2
fi
IFS=. read -r major minor patch <<<"$version"

git fetch --quiet --tags origin

# 2. 分支名必须与版本一致
branch=$(git symbolic-ref --short -q HEAD || echo "")
case "$branch" in
  "release/$version") kind=release ;;
  "hotfix/$version")  kind=hotfix ;;
  *) err "当前分支是 '${branch:-detached}'，应在 release/$version 或 hotfix/$version 上发版" ; kind="" ;;
esac
[ -n "$kind" ] && ok "分支 $branch"

# 3. 工作区干净
if [ -n "$(git status --porcelain)" ]; then
  err "工作区有未提交改动"
else
  ok "工作区干净"
fi

# 4. 与远端同步（远端分支还不存在时只提示，首次推送前也能跑）
if [ -z "$kind" ]; then
  :  # 分支已判错，不再比对远端
elif git rev-parse -q --verify "origin/$branch" >/dev/null 2>&1; then
  git fetch --quiet origin "$branch"
  if [ "$(git rev-parse HEAD)" != "$(git rev-parse "origin/$branch")" ]; then
    err "本地 $branch 与 origin/$branch 不一致，先 push / pull"
  else
    ok "与 origin/$branch 同步"
  fi
else
  warn "origin/$branch 尚不存在，核查通过后记得 git push -u origin $branch"
fi

# 5. tag 未被占用
if git rev-parse -q --verify "refs/tags/v$version" >/dev/null; then
  err "tag v$version 已存在（v* tag 不可删改，只能换下一个版本号）"
else
  ok "tag v$version 未占用"
fi

# 6. 版本号大于上一个正式 tag
prev=$(git tag --list 'v[0-9]*' | grep -vE -- '-' | sed 's/^v//' | sort -V | tail -1 || true)
if [ -n "$prev" ]; then
  top=$(printf '%s\n%s\n' "$prev" "$version" | sort -V | tail -1)
  if [ "$top" != "$version" ] || [ "$prev" = "$version" ]; then
    err "版本 $version 不大于上一个正式版 $prev"
  else
    ok "版本 $prev → $version"
  fi
fi

# 7. CHANGELOG：本版本段落带日期，[Unreleased] 已清空
if grep -qE "^## \[$version\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$" CHANGELOG.md; then
  ok "CHANGELOG 有 ## [$version] - <日期>"
else
  err "CHANGELOG 缺少 '## [$version] - YYYY-MM-DD' 段落"
fi
unreleased=$(awk '
  /^## \[Unreleased\]/ { inside = 1; next }
  inside && /^## \[/    { exit }
  inside && /^- /       { n++ }
  END                   { print n + 0 }
' CHANGELOG.md)
if [ "$unreleased" -gt 0 ]; then
  err "CHANGELOG 的 [Unreleased] 下还有 $unreleased 条，应移进 [$version]"
else
  ok "[Unreleased] 已清空"
fi
if ! grep -qF "[$version]: " CHANGELOG.md; then
  err "CHANGELOG 文末缺少 [$version]: 链接，[Unreleased] 对比链接也要改成从 v$version 起"
fi

# 8. 有新迁移：至少 MINOR、写了升级须知；hotfix 不得带迁移
if [ -n "$prev" ]; then
  new_migrations=$(git diff --name-only --diff-filter=A "v$prev"..HEAD -- 'migrations/*.up.sql' | wc -l)
  if [ "$new_migrations" -gt 0 ]; then
    IFS=. read -r pmajor pminor _ <<<"$prev"
    if [ "$kind" = hotfix ]; then
      err "hotfix 带了 $new_migrations 个新迁移 —— hotfix 禁止新增迁移，改走正常发版"
    elif [ "$major" = "$pmajor" ] && [ "$minor" = "$pminor" ]; then
      err "自 v$prev 起新增 $new_migrations 个迁移，版本至少要升 MINOR（当前只升了 PATCH）"
    else
      ok "新增 $new_migrations 个迁移，版本已升 MINOR 及以上"
    fi
    section=$(awk -v v="## [$version]" '
      index($0, v) == 1 { inside = 1; next }
      inside && /^## \[/ { exit }
      inside { print }
    ' CHANGELOG.md)
    if ! printf '%s' "$section" | grep -q '^### 升级须知'; then
      err "有新迁移，但 [$version] 段落里没有 '### 升级须知'"
    fi
  else
    ok "无新迁移"
  fi
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "就绪度核查未通过。" >&2
  exit 1
fi
echo "就绪度核查通过。下一步：git tag -a v$version-rc.1 -m \"Lathe v$version-rc.1\" && git push origin v$version-rc.1"
