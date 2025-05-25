#!/usr/bin/env bash
# testdata/test_executable.sh
set -euo pipefail

while IFS= read -r line; do
	[[ -z $line ]] && continue

	id=$(jq -e -r '.id' <<<"$line" 2>/dev/null || true)
	[[ -z $id ]] && continue

	method=$(jq -r '.method // empty' <<<"$line")
	data=$(jq -c '.data // {}'        <<<"$line")

	case "$method" in
		add)
			# Both fields must be JSON numbers, not strings.
			if ! sum=$(jq -e '
					if (.a|type=="number") and (.b|type=="number")
					then (.a + .b)
					else error("invalid")
					end
				' <<<"$data" 2>/dev/null); then
				printf '{"id":"%s","err":"invalid payload"}\n' "$id"
				continue
			fi
			printf '{"id":"%s","data":{"sum":%s}}\n' "$id" "$sum"
			;;
		*)
			printf '{"id":"%s","err":"unknown method: %s"}\n' "$id" "$method"
			;;
	esac
done
