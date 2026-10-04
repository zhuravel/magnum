// Package magnum holds what the binary embeds from the repository root.
package magnum

import _ "embed"

// DefaultConfig is config.defaults.toml: the built-in configuration (agent
// kinds, roles, daemon defaults, every key documented). The user's
// ~/.config/magnum/config.toml layers over it.
//
//go:embed config.defaults.toml
var DefaultConfig []byte

// Skill is skills/magnum-review/SKILL.md: the judge's skill, which an
// installed binary (no checkout) hands to its judges through the daemon's
// startup copy (config.EmbeddedSkill).
//
//go:embed skills/magnum-review/SKILL.md
var Skill []byte

// PluginManifest and PluginScript are the herdr plugin (herdr-plugin.toml,
// scripts/magnum-ctl.sh), which `magnum install --plugin` writes out for an
// installed binary to link (a checkout links itself).
//
//go:embed herdr-plugin.toml
var PluginManifest []byte

//go:embed scripts/magnum-ctl.sh
var PluginScript []byte
