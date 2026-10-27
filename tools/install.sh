#!/bin/bash
# install.sh [утилита…] — установка утилит akit в ~/bin только из чистого дерева. Без аргументов — все из cmd/.
# Сборка в <имя>.next, подмена mv. Печатает vcs.revision установленного.
# Причина: бинарник, собранный до коммита (vcs.modified=true), по git не опознать.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
if [ -n "$(git -C "$repo" status --porcelain)" ]; then
	echo "install: dirty tree, commit first (graft commit):" >&2
	git -C "$repo" status --short >&2
	exit 1
fi
cmds=("$@")
if [ ${#cmds[@]} -eq 0 ]; then
	for d in "$repo"/cmd/*/; do
		cmds+=("$(basename "$d")")
	done
fi
for c in "${cmds[@]}"; do
	if [ ! -d "$repo/cmd/$c" ]; then
		echo "install: no tool $c in cmd/" >&2
		exit 1
	fi
	dst=$HOME/bin/$c
	go -C "$repo" build -o "$dst.next" "./cmd/$c"
	if go version -m "$dst.next" | grep -q 'vcs.modified=true'; then
		rm -f "$dst.next"
		echo "install: $c build is marked modified, not installed" >&2
		exit 1
	fi
	mv "$dst.next" "$dst"
	echo "$c $(go version -m "$dst" | sed -n 's/.*vcs.revision=\(.......\).*/\1/p')"
done
