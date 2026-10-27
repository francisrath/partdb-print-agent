// Package assets embeds static files served by the agent.
package assets

import "embed"

// Web holds the favicon set for the local web UI (icon/web/…). Regenerate with scripts/gen-icons.sh.
//
//go:embed icon/web
var Web embed.FS
