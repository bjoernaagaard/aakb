// Package assets embeds the aakb artwork.
package assets

import _ "embed"

// Icon is the SVG aakb mark.
//
//go:embed icon.svg
var Icon []byte
