#!/usr/bin/env bash
# Lists a provider's resource and data source types that are not yet in
# catalog/<provider>.json, one per line: kind<TAB>type<TAB>required attrs<TAB>reference-like attrs.
#
# usage: scripts/provider-types.sh <provider> <initialized terraform dir> [type-prefix]
# e.g.   scripts/provider-types.sh aws examples/lambda-api-gateway aws_sqs_
set -euo pipefail

provider=$1 dir=$2 prefix=${3:-}
root=$(cd "$(dirname "$0")/.." && pwd)
catalog="$root/catalog/$provider.json"
[ -f "$catalog" ] || catalog=/dev/null

terraform -chdir="$dir" providers schema -json | jq -r \
  --arg p "$provider" --arg prefix "$prefix" --slurpfile cat <(cat "$catalog" 2>/dev/null || echo '{}') '
  ($cat[0].resources // {}) as $res | ($cat[0].data_sources // {}) as $data |
  .provider_schemas | to_entries[] | select(.key | endswith("/" + $p)) | .value as $s |
  def rows(kind; known):
    to_entries[] | select(.key | startswith($prefix)) | select(known[.key] == null) |
    (.value.block.attributes // {}) as $a |
    [kind, .key,
     ([$a | to_entries[] | select(.value.required) | .key] | join(",")),
     ([$a | keys[] | select(test("(_id|_arn|_name|_url|^role|^bucket|^function_name|^target|^endpoint|^source_arn)$"))] | join(","))]
    | @tsv;
  ($s.resource_schemas // {} | rows("resource"; $res)),
  ($s.data_source_schemas // {} | rows("data"; $data))'
