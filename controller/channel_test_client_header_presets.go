package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Built-in client header presets offered to admins as a dropdown.
//
// Every version here was taken from the package registry on 2026-08-02 (npm for
// the CLIs, PyPI for the SDKs) rather than invented, and the user-agent
// templates were read out of the shipped client itself where one was available
// locally — claude-cli's builder is
//
//	`claude-cli/${VERSION}` + ` (${[entrypoint, ...extras].join(", ")})`
//
// which is why the Claude presets carry a parenthesised, comma-separated list
// rather than a single word.
//
// A preset is a starting point, not a guarantee: upstreams that gate on a
// minimum client version move, and the newest entry here goes stale the moment
// a client ships. That is the whole reason this is a dropdown over an editable
// field instead of a fixed constant — pick the closest one, then edit.

// clientHeaderPreset is one selectable entry.
type clientHeaderPreset struct {
	// ID is the stable key the frontend sends back; it must not change once
	// released or saved selections break.
	ID string `json:"id"`
	// Label is what the dropdown shows.
	Label string `json:"label"`
	// Family groups presets in the dropdown (claude / openai / codex / gemini).
	Family string `json:"family"`
	// Endpoint is the endpoint type this client actually talks to, so the UI can
	// warn when a preset is paired with a channel that cannot serve it.
	Endpoint string `json:"endpoint"`
	// Headers is the full set applied when this preset is chosen.
	Headers map[string]string `json:"headers"`
}

func claudeCodePreset(version string) map[string]string {
	return map[string]string{
		"user-agent":        "claude-cli/" + version + " (external, cli)",
		"x-app":             "cli",
		"anthropic-version": "2023-06-01",
		"accept-language":   "*",
	}
}

func openAIPythonPreset(version string) map[string]string {
	return map[string]string{
		"user-agent":                  "OpenAI/Python " + version,
		"x-stainless-lang":            "python",
		"x-stainless-package-version": version,
		"x-stainless-runtime":         "CPython",
		"x-stainless-runtime-version": "3.12.3",
		"x-stainless-os":              "Linux",
		"x-stainless-arch":            "x64",
		"accept-language":             "*",
	}
}

func codexPreset(version string) map[string]string {
	return map[string]string{
		"user-agent":      "codex_cli_rs/" + version + " (Linux 6.8.0; x86_64) terminal",
		"originator":      "codex_cli_rs",
		"accept-language": "*",
	}
}

func googleGenAIPreset(version string) map[string]string {
	agent := "google-genai-sdk/" + version + " gl-python/3.12.3"
	return map[string]string{
		"user-agent":        agent,
		"x-goog-api-client": agent,
		"accept-language":   "*",
	}
}

// builtinClientHeaderPresets is ordered newest-first within each family, which
// is the order the dropdown renders.
var builtinClientHeaderPresets = []clientHeaderPreset{
	// Claude Code CLI — npm @anthropic-ai/claude-code, versions as published.
	{ID: "claude-code-2.1.220", Label: "Claude Code CLI 2.1.220 (latest)", Family: "claude", Endpoint: "anthropic", Headers: claudeCodePreset("2.1.220")},
	{ID: "claude-code-2.1.216", Label: "Claude Code CLI 2.1.216", Family: "claude", Endpoint: "anthropic", Headers: claudeCodePreset("2.1.216")},
	{ID: "claude-code-2.1.210", Label: "Claude Code CLI 2.1.210", Family: "claude", Endpoint: "anthropic", Headers: claudeCodePreset("2.1.210")},
	{ID: "claude-code-2.1.206", Label: "Claude Code CLI 2.1.206", Family: "claude", Endpoint: "anthropic", Headers: claudeCodePreset("2.1.206")},
	{ID: "claude-code-2.0.14", Label: "Claude Code CLI 2.0.14 (older)", Family: "claude", Endpoint: "anthropic", Headers: claudeCodePreset("2.0.14")},

	// openai-python — PyPI openai.
	{ID: "openai-python-2.52.0", Label: "openai-python 2.52.0 (latest)", Family: "openai", Endpoint: "openai", Headers: openAIPythonPreset("2.52.0")},
	{ID: "openai-python-2.50.0", Label: "openai-python 2.50.0", Family: "openai", Endpoint: "openai", Headers: openAIPythonPreset("2.50.0")},
	{ID: "openai-python-2.45.0", Label: "openai-python 2.45.0", Family: "openai", Endpoint: "openai", Headers: openAIPythonPreset("2.45.0")},
	{ID: "openai-python-2.40.0", Label: "openai-python 2.40.0", Family: "openai", Endpoint: "openai", Headers: openAIPythonPreset("2.40.0")},
	{ID: "openai-python-1.99.1", Label: "openai-python 1.99.1 (older)", Family: "openai", Endpoint: "openai", Headers: openAIPythonPreset("1.99.1")},

	// Codex CLI — npm @openai/codex. Talks to /v1/responses, not chat completions.
	{ID: "codex-cli-0.146.0", Label: "Codex CLI 0.146.0 (latest) — /v1/responses", Family: "codex", Endpoint: "openai-response", Headers: codexPreset("0.146.0")},
	{ID: "codex-cli-0.145.0", Label: "Codex CLI 0.145.0 — /v1/responses", Family: "codex", Endpoint: "openai-response", Headers: codexPreset("0.145.0")},
	{ID: "codex-cli-0.144.1", Label: "Codex CLI 0.144.1 — /v1/responses", Family: "codex", Endpoint: "openai-response", Headers: codexPreset("0.144.1")},
	{ID: "codex-cli-0.143.0", Label: "Codex CLI 0.143.0 — /v1/responses", Family: "codex", Endpoint: "openai-response", Headers: codexPreset("0.143.0")},

	// google-genai — PyPI google-genai.
	{ID: "google-genai-2.16.0", Label: "google-genai 2.16.0 (latest)", Family: "gemini", Endpoint: "gemini", Headers: googleGenAIPreset("2.16.0")},
	{ID: "google-genai-2.14.0", Label: "google-genai 2.14.0", Family: "gemini", Endpoint: "gemini", Headers: googleGenAIPreset("2.14.0")},
	{ID: "google-genai-2.12.0", Label: "google-genai 2.12.0", Family: "gemini", Endpoint: "gemini", Headers: googleGenAIPreset("2.12.0")},
	{ID: "google-genai-2.9.0", Label: "google-genai 2.9.0 (older)", Family: "gemini", Endpoint: "gemini", Headers: googleGenAIPreset("2.9.0")},
}

// GetChannelTestClientHeaderPresets serves the preset list to the admin UI so
// the dropdown never drifts from what the backend would actually send.
func GetChannelTestClientHeaderPresets() []clientHeaderPreset {
	return builtinClientHeaderPresets
}

// ListChannelTestClientHeaderPresets exposes the presets over HTTP. The UI reads
// them rather than shipping its own copy: two hardcoded lists in two languages
// drift, and a preset that does not match what the backend sends is worse than
// no preset at all.
func ListChannelTestClientHeaderPresets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    GetChannelTestClientHeaderPresets(),
	})
}
