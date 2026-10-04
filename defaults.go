// Package magnum holds what the binary embeds from the repository root.
package magnum

import _ "embed"

// DefaultConfig is config.defaults.toml: the built-in configuration (agent
// kinds, roles, daemon defaults, every key documented). The user's
// ~/.config/magnum/config.toml layers over it.
//
//go:embed config.defaults.toml
var DefaultConfig []byte
