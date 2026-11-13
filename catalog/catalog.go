// Package catalog embeds the per-provider catalogs: the architectural role of
// each resource and data source type (see aws.json for the format).
package catalog

import "embed"

//go:embed *.json
var FS embed.FS
