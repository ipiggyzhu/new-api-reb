package constant

// Client header families name the synthesized client profile a request wears:
// the set of headers a real client of that kind actually sends.
//
// These strings are a stored contract in three places — the outer keys of the
// admin's channel-test header override map, the Family field of a built-in
// preset, and a channel's synthetic_client_headers_profile — so renaming one
// silently drops whatever is already configured under the old name.
//
// They live here rather than next to the profiles in relay/channel because dto
// needs them to normalize the per-channel setting, and dto cannot import
// relay/channel without a cycle. relay/channel re-exports them, so call sites
// there are unchanged.
const (
	ClientHeaderFamilyClaude  = "claude"
	ClientHeaderFamilyOpenAI  = "openai"
	ClientHeaderFamilyCodex   = "codex"
	ClientHeaderFamilyGemini  = "gemini"
	ClientHeaderFamilyGeneric = "generic"
	// ClientHeaderFamilyAll applies to every family, for headers an admin wants
	// on all requests regardless of channel type.
	ClientHeaderFamilyAll = "*"
)

// ClientHeaderFamilies lists the families a channel can be dressed as, in the
// order the admin UI shows them. ClientHeaderFamilyAll is deliberately absent:
// it is an override scope, not a profile anything can wear.
var ClientHeaderFamilies = []string{
	ClientHeaderFamilyClaude,
	ClientHeaderFamilyOpenAI,
	ClientHeaderFamilyCodex,
	ClientHeaderFamilyGemini,
	ClientHeaderFamilyGeneric,
}

// IsClientHeaderFamily reports whether family names a real profile. Callers use
// it to reject a typo rather than let it fall through to the generic profile,
// which would look like it worked.
func IsClientHeaderFamily(family string) bool {
	for _, known := range ClientHeaderFamilies {
		if known == family {
			return true
		}
	}
	return false
}
