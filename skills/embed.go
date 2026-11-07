// Package skills embeds the installable agent skills.
package skills

import "embed"

// FS holds one directory per skill, each with a SKILL.md.
//
//go:embed k8s-axi
var FS embed.FS
