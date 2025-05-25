#!/usr/bin/env bash
# testdata/test_executable.sh
set -euo pipefail

while IFS= read -r line; do
	[[ -z $line ]] && continue

	# Handle cancellation envelopes
	cancel=$(jq -r '.cancel // empty' <<<"$line" 2>/dev/null || true)
	if [[ -n $cancel ]]; then
		# Host is telling us to stop work on <cancel>; nothing to return.
		continue
	fi

	# Regular request.
	id=$(jq -e -r '.id' <<<"$line" 2>/dev/null || true)
	[[ -z $id ]] && continue   # Malformed – ignore.

	method=$(jq -r '.method // empty' <<<"$line")
	data=$(jq  -c '.data   // {}'    <<<"$line")

	case "$method" in
	  add)
		# Both fields must be JSON numbers.
		if ! sum=$(jq -e '
			 if (.a|type=="number") and (.b|type=="number")
			 then (.a + .b) else error("invalid") end
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