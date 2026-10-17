#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 jdominick05
#
# llm-top installer for Linux: downloads a release, checks it against SHA256SUMS, installs to ~/.local/bin.
#
#   curl -fsSL https://raw.githubusercontent.com/jdominick05/llm-top/main/install.sh | sh
#   sh install.sh [version]           # e.g. 0.2.0, default: latest release
#
# Env: LLMTOP_BIN (install dir, default ~/.local/bin), LLMTOP_VERSION, GITHUB_TOKEN (optional: higher API rate
# limit), LLMTOP_GH=1 (download with a logged-in gh instead). Needs curl (or gh), tar, sha256sum.
# Uninstall: rm ~/.local/bin/llm-top (and ~/.config/llm-top).
set -eu

repo=jdominick05/llm-top

die() {
	echo "install.sh: $*" >&2
	exit 1
}

# api URL -> $tmp/release.json; prints the HTTP status
fetch_release() {
	curl -sSL -H "Accept: application/vnd.github+json" ${auth:+-H "$auth"} -w '%{http_code}' -o "$tmp/release.json" "$1" ||
		die "cannot reach api.github.com"
}

# everything runs from main, called on the last line: a download cut short by `curl | sh` runs nothing
main() {
	ver=${1:-${LLMTOP_VERSION:-latest}}
	ver=${ver#v}
	bin=${LLMTOP_BIN:-$HOME/.local/bin}

	[ "$(uname -s)" = Linux ] || die "this is the Linux installer (got $(uname -s)); on Windows use install.ps1"
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	*) die "no prebuilt binary for $(uname -m); build from source: go build -o llm-top ./src" ;;
	esac
	command -v tar >/dev/null || die "tar is missing"
	command -v sha256sum >/dev/null || die "sha256sum is missing"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	if [ -n "${LLMTOP_GH:-}" ] && command -v gh >/dev/null; then
		tag=
		[ "$ver" = latest ] || tag="v$ver"
		# shellcheck disable=SC2086
		gh release download $tag -R "$repo" -p "llm-top-*-linux-$arch.tar.gz" -p SHA256SUMS -D "$tmp" ||
			die "gh release download failed"
	else
		command -v curl >/dev/null || die "curl is missing"
		api="https://api.github.com/repos/$repo/releases/latest"
		[ "$ver" = latest ] || api="https://api.github.com/repos/$repo/releases/tags/v$ver"
		auth=
		[ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
		code=$(fetch_release "$api")
		if [ "$code" = 401 ] && [ -n "$auth" ]; then # a stale token: the repo is public, try without
			auth=
			code=$(fetch_release "$api")
		fi
		case $code in
		200) ;;
		404) die "no release ${tag:-$ver} (see https://github.com/$repo/releases)" ;;
		403 | 429) die "GitHub API rate limit reached; try again in a while or set GITHUB_TOKEN" ;;
		*) die "GitHub API answered $code for $api" ;;
		esac
		# asset objects list "name" before "browser_download_url"; remember the name, print the url when it matches
		asset_url() {
			awk -v want="$1" '
				/"name": *"/ { n = $0; sub(/.*"name": *"/, "", n); sub(/".*/, "", n) }
				/"browser_download_url": *"/ { u = $0; sub(/.*"browser_download_url": *"/, "", u); sub(/".*/, "", u); if (n == want) { print u; exit } }
			' "$tmp/release.lines"
		}
		tr ',{}' '\n\n\n' <"$tmp/release.json" >"$tmp/release.lines" # one field per line, pretty-printed or not
		tag=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.lines" | head -n 1)
		[ -n "$tag" ] || die "no release found"
		tgz="llm-top-${tag#v}-linux-$arch.tar.gz"
		for f in "$tgz" SHA256SUMS; do
			u=$(asset_url "$f")
			[ -n "$u" ] || die "release $tag has no $f"
			curl -fsSL "$u" -o "$tmp/$f" || die "download of $f failed"
		done
	fi

	tgz=$(cd "$tmp" && ls llm-top-*-linux-"$arch".tar.gz 2>/dev/null | head -n 1)
	[ -n "$tgz" ] || die "download incomplete"
	(cd "$tmp" && grep "  $tgz\$" SHA256SUMS | sha256sum -c -) >/dev/null || die "checksum mismatch for $tgz"
	mkdir -p "$tmp/x" "$bin"
	tar -xzf "$tmp/$tgz" -C "$tmp/x" llm-top
	# write next to the target and rename, so a running llm-top is replaced safely
	cp "$tmp/x/llm-top" "$bin/.llm-top.new"
	chmod 755 "$bin/.llm-top.new"
	mv -f "$bin/.llm-top.new" "$bin/llm-top"

	echo "installed $("$bin/llm-top" -version) to $bin/llm-top"
	case ":$PATH:" in
	*":$bin:"*) ;;
	*) echo "note: $bin is not on PATH; add it, e.g.  echo 'export PATH=\"$bin:\$PATH\"' >> ~/.profile" ;;
	esac
	cfg=${XDG_CONFIG_HOME:-$HOME/.config}/llm-top/llm-top.json
	echo "config: $cfg (written with defaults on first run; set server_url / api_key there)"
}

main "$@"
