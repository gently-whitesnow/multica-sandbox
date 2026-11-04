# Creates or fetches a bare reference mirror (ADR 0035) through the Git relay. It runs in a
# controller-owned container that receives no attempt input beyond the admitted URL.
# Arguments: directory url.
set -eu
dir=$1 url=$2
fetch() {
	git -C "$1" -c core.hooksPath=/dev/null -c gc.autoDetach=false -c maintenance.autoDetach=false \
		fetch -q --prune --no-tags -- "$url" '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*' &&
		rm -f -- "$1/FETCH_HEAD"
}
if [ -d "$dir" ]; then
	# Refreshes of a mirror never overlap, so lock files are left by a cut one.
	find "$dir" -name '*.lock' -type f -exec rm -f -- {} +
	fetch "$dir" && exit 0
	# An unreachable origin keeps the mirror; one Git cannot read is rebuilt.
	git -C "$dir" fsck --no-progress --connectivity-only --no-dangling >/dev/null 2>&1 && exit 1
	echo "mirror unreadable, rebuilding" >&2
fi
rm -rf -- "$dir.new" "$dir.old"
git init -q --bare "$dir.new"
fetch "$dir.new"
[ ! -d "$dir" ] || mv -- "$dir" "$dir.old"
mv -- "$dir.new" "$dir"
rm -rf -- "$dir.old"
