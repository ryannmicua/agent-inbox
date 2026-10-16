#!/usr/bin/env bash
# Build release archives for agent-inbox, inboxd, and agent-inbox-mcp on this
# machine, then tag the release and publish it on GitHub with gh.
#
#   scripts/release.sh --dry-run v0.1.0   build and checksum into dist/v0.1.0 only
#   scripts/release.sh v0.1.0             build, tag, push the tag, and publish
#
# Each archive holds the three programs with LICENSE and README.md. A release
# must run from a clean checkout of the default branch at the same commit as
# origin, with LICENSE present. A dry run reports those checks as warnings and
# continues.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

usage() {
  echo "usage: $0 [--dry-run] vMAJOR.MINOR.PATCH" >&2
  exit 2
}

dry_run=0
version=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) dry_run=1 ;;
    -h|--help) usage ;;
    -*) echo "unknown option: $arg" >&2; usage ;;
    *) [[ -z "$version" ]] || usage; version="$arg" ;;
  esac
done
[[ -n "$version" ]] || usage
if [[ ! "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "version must be a semver tag such as v0.1.0, got: $version" >&2
  exit 2
fi

programs=(agent-inbox inboxd agent-inbox-mcp)
targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)
version_var="github.com/ryannmicua/agent-inbox/internal/inbox.version"
tar_cmd="${TAR:-tar}"

for tool in git go zip "$tar_cmd"; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done
"$tar_cmd" --version 2>/dev/null | grep -q 'GNU tar' || {
  echo "GNU tar is required; set TAR to its path, for example TAR=gtar" >&2
  exit 1
}
if command -v sha256sum >/dev/null; then
  sha256=(sha256sum)
elif command -v shasum >/dev/null; then
  sha256=(shasum -a 256)
else
  echo "sha256sum or shasum is required" >&2
  exit 1
fi
if (( ! dry_run )); then
  command -v gh >/dev/null || { echo "gh is required to publish" >&2; exit 1; }
fi

problems=0
check_failed() {
  if (( dry_run )); then
    echo "warning: $1 (a release would stop here)" >&2
  else
    echo "error: $1" >&2
  fi
  problems=$((problems + 1))
}

origin_url="$(git remote get-url origin)"
if [[ "$origin_url" =~ github\.com[:/]([^/]+/[^/]+)$ ]]; then
  repo="${BASH_REMATCH[1]%.git}"
else
  echo "origin is not a GitHub repository: $origin_url" >&2
  exit 1
fi

default_branch="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
default_branch="${default_branch#origin/}"
default_branch="${default_branch:-main}"

# Every archive carries these files beside the programs.
bundled_files=(LICENSE README.md)
for file in "${bundled_files[@]}"; do
  [[ -f "$file" ]] || check_failed "$file is missing; every archive must include it"
done

if [[ -n "$(git status --porcelain)" ]]; then
  check_failed "the working tree has uncommitted or untracked changes"
fi
current_branch="$(git symbolic-ref --quiet --short HEAD || true)"
if [[ "$current_branch" != "$default_branch" ]]; then
  check_failed "HEAD is on '${current_branch:-detached}', not the default branch '$default_branch'"
fi
head_commit="$(git rev-parse HEAD)"
remote_commit="$(git ls-remote origin "refs/heads/$default_branch" | cut -f1 || true)"
if [[ "$head_commit" != "$remote_commit" ]]; then
  check_failed "HEAD ${head_commit:0:12} is not origin/$default_branch ${remote_commit:0:12}; pull or push first"
fi
if git rev-parse --quiet --verify "refs/tags/$version" >/dev/null; then
  check_failed "tag $version already exists locally"
fi
if git ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1; then
  check_failed "tag $version already exists on origin"
fi
if (( ! dry_run )); then
  gh auth status >/dev/null 2>&1 || check_failed "gh is not logged in; run gh auth login"
  if gh release view "$version" --repo "$repo" >/dev/null 2>&1; then
    check_failed "release $version already exists on $repo"
  fi
fi
if (( problems && ! dry_run )); then
  exit 1
fi

out_dir="$repo_root/dist/$version"
stage_dir="$(mktemp -d)"
trap 'rm -rf "$stage_dir"' EXIT
rm -rf "$out_dir"
mkdir -p "$out_dir"

# Archive entries carry the commit time and no owner names, so rebuilding the
# same commit with the same Go toolchain gives the same archives.
commit_time="$(git log -1 --format=%ct HEAD)"
touch_stamp="$(TZ=UTC0 git log -1 --date=format-local:%Y%m%d%H%M.%S --format=%cd HEAD)"

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  name="agent-inbox_${goos}_${goarch}"
  ext=""
  [[ "$goos" == windows ]] && ext=".exe"
  mkdir -p "$stage_dir/$name"
  for program in "${programs[@]}"; do
    echo "build $goos/$goarch $program"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
      -ldflags "-s -w -X $version_var=$version" \
      -o "$stage_dir/$name/$program$ext" "./cmd/$program"
  done
  for file in "${bundled_files[@]}"; do
    [[ ! -f "$file" ]] || cp "$file" "$stage_dir/$name/$file"
  done
  find "$stage_dir/$name" -exec env TZ=UTC0 touch -h -t "$touch_stamp" {} +
  if [[ "$goos" == windows ]]; then
    (cd "$stage_dir" && zip -q -r -X "$out_dir/$name.zip" "$name")
  else
    "$tar_cmd" -C "$stage_dir" --sort=name --owner=0 --group=0 --numeric-owner \
      --mtime="@$commit_time" -cf - "$name" | gzip -n -9 >"$out_dir/$name.tar.gz"
  fi
done

(cd "$out_dir" && "${sha256[@]}" -- *.tar.gz *.zip >SHA256SUMS && "${sha256[@]}" -c --quiet SHA256SUMS)

# Run this machine's own build to prove the version stamp took.
host_target="$(go env GOOS)/$(go env GOARCH)"
for target in "${targets[@]}"; do
  [[ "$target" == "$host_target" && "$target" != windows/* ]] || continue
  host_dir="$stage_dir/agent-inbox_${target%/*}_${target#*/}"
  for program in "${programs[@]}"; do
    if [[ "$program" == agent-inbox-mcp ]]; then
      got="$("$host_dir/$program" --version)"
    else
      got="$("$host_dir/$program" version)"
    fi
    if [[ "$got" != "$program $version" ]]; then
      echo "error: $program reports '$got', want '$program $version'" >&2
      exit 1
    fi
  done
  echo "checked: the $target build reports $version"
done

echo
echo "artifacts in $out_dir:"
cat "$out_dir/SHA256SUMS"

if (( dry_run )); then
  echo
  echo "dry run: no tag created and nothing published"
  exit 0
fi

git tag -a "$version" -m "agent-inbox $version"
git push origin "refs/tags/$version"
notes="Prebuilt agent-inbox, inboxd, and agent-inbox-mcp for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, and windows/amd64. Check each download against SHA256SUMS. Install steps: https://github.com/$repo/blob/$version/docs/install.md#download-a-release-binary"
if ! gh release create "$version" --repo "$repo" --verify-tag \
  --title "agent-inbox $version" --notes "$notes" --generate-notes \
  "$out_dir"/*.tar.gz "$out_dir"/*.zip "$out_dir/SHA256SUMS"; then
  echo "error: tag $version is pushed but the release was not created." >&2
  echo "Retry with: gh release create $version --repo $repo --verify-tag --generate-notes $out_dir/*" >&2
  exit 1
fi
echo "published https://github.com/$repo/releases/tag/$version"
