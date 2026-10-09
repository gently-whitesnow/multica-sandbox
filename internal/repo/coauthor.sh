# Upstream Co-authored-by state and hooks (Multica b4ca5b4 repocache: WriteCoAuthoredByState,
# reconcileHookAt, installCoAuthoredByHook, removeCoAuthoredByHook); $hooktext is the hook.
ours() { grep -qF -e '# multica:prepare-commit-msg:co-authored-by' -e '# Installed by the Multica daemon.' "$1" 2>/dev/null; }
# hook <hooks-dir> <enabled> <replace-foreign>: installs or removes the trailer hook.
hook() {
	h=$1/prepare-commit-msg
	if [ "$2" != 1 ]; then
		! ours "$h" || rm -f -- "$h"
	elif [ "$3" = 1 ] || [ ! -e "$h" ] || ours "$h"; then
		mkdir -p "$1" && printf '%s' "$hooktext" >"$h.tmp" && chmod 755 "$h.tmp" && mv -f "$h.tmp" "$h"
	fi
}
# publish <enabled> <state> <workdir> <hook>: records the setting the hooks read at commit
# time, then reconciles checkouts up to two levels below the workdir (upstream sweep).
publish() {
	hooktext=$4
	printf '%s\n' "$1" >"$2.tmp" && mv -f "$2.tmp" "$2" || exit 1
	for d in "$3"/*; do
		[ -L "$d" ] && continue
		if [ -d "$d/.git" ]; then
			hook "$d/.git/hooks" "$1" 0
			continue
		fi
		for e in "$d"/*; do
			[ ! -L "$e" ] && [ -d "$e/.git" ] && hook "$e/.git/hooks" "$1" 0
		done
	done
	exit 0
}
