# Upstream isolated checkout (Multica b4ca5b4 repocache: ensureIsolatedCheckout,
# inspectExistingCheckout, resolveRef, excludeFromGit, prepareCommitMsgHook), cloned
# from origin through the Git relay instead of a bare cache. Runs as the attempt user;
# it prints result lines, or "error <kind> <message>". Arguments: url ref branch fresh
# workdir name root commit-name commit-email co-author.
set -u
fail() { printf 'error %s %s\n' "$1" "$(printf '%s' "$2" | tr '\r\n' '  ' | cut -c1-600)"; exit 1; }
run() { out=$("$@" 2>&1) || fail failed "$(printf '%s\n' "$out" | tail -n 3)"; }
url=$1 ref=$2 branch=$3 fresh=$4 name=$6 root=$7 author=$8 email=$9 coauthor=${10}
dir=$(cd -P -- "$5" 2>/dev/null && pwd -P) || fail forbidden "resolve requested workdir: $5"
case "$dir" in "$root" | "$root"/*) ;; *) fail forbidden "$dir is outside the active task workdir $root" ;; esac
path=$dir/$name
commit() { git -C "$path" rev-parse --verify --quiet --end-of-options "$1^{commit}" 2>/dev/null; }
resolve() {
	if [ -n "$ref" ]; then
		for c in "refs/remotes/origin/$ref" "refs/tags/$ref" "$ref"; do commit "$c" && return; done
		fail failed "cannot resolve requested ref \"$ref\""
	fi
	for c in $(git -C "$path" symbolic-ref --quiet refs/remotes/origin/HEAD 2>/dev/null) refs/remotes/origin/main refs/remotes/origin/master; do
		commit "$c" && return
	done
	only=$(git -C "$path" for-each-ref --format='%(refname)' refs/remotes/origin/ | grep -vx refs/remotes/origin/HEAD)
	[ "$(printf '%s' "$only" | grep -c .)" = 1 ] && commit "$only" && return
	fail failed "cannot resolve default branch"
}
exclude() {
	file=$(git -C "$path" rev-parse --absolute-git-dir)/info/exclude
	mkdir -p "${file%/*}"
	for p in .agent_context CLAUDE.md AGENTS.md .claude .opencode .codeartsdoer .deveco CODEBUDDY.md .codebuddy .pi .omp; do
		grep -qF -- "$p" "$file" 2>/dev/null || printf '\n%s\n' "$p" >>"$file"
	done
}
# configure stands in for the native host's global identity and reconciles the hook.
configure() {
	run git -C "$path" config user.name "$author"
	run git -C "$path" config user.email "$email"
	hook=$(git -C "$path" rev-parse --absolute-git-dir)/hooks/prepare-commit-msg
	if [ "$coauthor" = 1 ]; then
		mkdir -p "${hook%/*}" && cat >"$hook.tmp" <<'HOOK' && chmod 755 "$hook.tmp" && mv -f "$hook.tmp" "$hook" || fail failed "install prepare-commit-msg hook"
#!/bin/sh
# multica:prepare-commit-msg:co-authored-by
# Multica: add Co-authored-by trailer for the Multica Agent.
# Installed by the Multica daemon. Do not edit — it will be overwritten.

COMMIT_MSG_FILE="$1"
COMMIT_SOURCE="$2"

# Skip merge and squash commits.
case "$COMMIT_SOURCE" in
  merge|squash) exit 0 ;;
esac

TRAILER="Co-authored-by: multica-agent <github@multica.ai>"

# Don't add if already present.
if grep -qF "$TRAILER" "$COMMIT_MSG_FILE"; then
  exit 0
fi

# Use git interpret-trailers for proper formatting.
git interpret-trailers --in-place --trailer "$TRAILER" "$COMMIT_MSG_FILE"
HOOK
	elif grep -qF '# multica:prepare-commit-msg:co-authored-by' "$hook" 2>/dev/null; then
		rm -f -- "$hook"
	fi
}
start() {
	if git -C "$path" show-ref --verify --quiet "refs/heads/$branch"; then branch=$branch-$(date +%s); fi
	run git -C "$path" checkout -q -b "$branch" "$base"
}
if [ -e "$path" ] || [ -L "$path" ]; then
	[ -d "$path/.git" ] && [ "$(git -C "$path" config --get multica.checkout-mode)" = isolated ] ||
		fail conflict "checkout path already exists and is not a Multica isolated checkout: $path"
	run git -C "$path" remote set-url origin "$url"
	run git -C "$path" fetch --force --no-tags origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
	git -C "$path" remote set-head origin --auto >/dev/null 2>&1
	current=$(git -C "$path" symbolic-ref --quiet --short HEAD 2>/dev/null) || current=
	uncommitted=$(git -C "$path" status --porcelain --untracked-files=all | grep -c .)
	unpushed=$(git -C "$path" rev-list --count HEAD --not --remotes 2>/dev/null) || unpushed=0
	kept=
	if [ "$fresh" != 1 ]; then
		suffix=${current#"$branch"-}
		if [ "$current" = "$branch" ] || { [ "$suffix" != "$current" ] && [ -n "$suffix" ] && [ -z "$(printf '%s' "$suffix" | tr -d 0-9)" ]; }; then
			kept=task_branch
		elif [ "$uncommitted" -gt 0 ] || [ "$unpushed" -gt 0 ]; then
			kept=local_work
		fi
	fi
	if [ -n "$kept" ]; then
		exclude
		configure
		printf 'path %s\nbranch %s\nkept %s\nuncommitted %s\nunpushed %s\n' "$path" "$current" "$kept" "$uncommitted" "$unpushed"
		exit 0
	fi
	base=$(resolve) || { printf '%s\n' "$base"; exit 1; }
	run git -C "$path" reset -q --hard
	run git -C "$path" clean -q -fd
	start
	for r in $(git -C "$path" for-each-ref --format='%(refname)' refs/heads/agent/); do
		[ "$r" != "refs/heads/$branch" ] && [ "$(git -C "$path" rev-list --count "$r" --not --remotes)" = 0 ] && git -C "$path" update-ref -d "$r"
	done
else
	trap 'rm -rf -- "$path"' EXIT
	run git clone -q --no-checkout -- "$url" "$path"
	base=$(resolve) || { printf '%s\n' "$base"; exit 1; }
	run git -C "$path" checkout -q --detach "$base"
	for r in $(git -C "$path" for-each-ref --format='%(refname)' refs/heads/); do run git -C "$path" update-ref -d "$r"; done
	run git -C "$path" config multica.checkout-mode isolated
	start
	trap - EXIT
fi
exclude
configure
printf 'path %s\nbranch %s\n' "$path" "$branch"
