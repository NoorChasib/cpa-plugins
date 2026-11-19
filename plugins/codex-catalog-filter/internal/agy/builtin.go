package agy

import (
	"hash/crc32"
	"strings"
)

// The tables below are copied from the agy 1.5.12 binary (Antigravity CLI,
// linux/amd64, file sha256 prefix 94104f028e6962fa), read from a copy with its
// pclntab and .rela.dyn; they are agy's own data, not CPA's. A later agy may
// change them: re-extract and compare before claiming a newer version.
//
// How agy 1.5.12 uses them for a Tier 2 (models.listSource) or Tier 3 list
// (gateway.buildResponseFromAdminModelList, codeassistclient.GatewayModelDetails,
// geminimodels.LookupModelInfo):
//
//   - The id sent to the gateway is wireAliases[id] when present, so
//     gemini-4-neon-xhigh is sent as gemini-4-neon.
//   - An id has built-in metadata (label, token limits, thinking level) when
//     LookupModelInfo finds it: by builtinIDEnum then infoTable's enum, else by
//     infoTable's name, trying the id with "-preview" added or removed.
//   - Image models (IsGatewayImageModel, or enums 1021, 1009 and 332) are not
//     offered.
//   - Every other id needs one of 32 placeholder enums, 1334 + slot, with slot
//     crc32.ChecksumIEEE(id) & 31 and linear probing. A built-in whose enum an
//     earlier entry already took needs a slot too. Ids beyond 32 slots are
//     dropped with "custom gateway model(s) exceeded the maximum placeholder
//     limit (32)".

// builtinIDEnum is geminimodels' id -> Model enum map (map.init.1, 36 keys).
var builtinIDEnum = map[string]int{
	"gemini-3.1-pro-preview":             1036,
	"gemini-3.1-pro":                     1036,
	"gemini-3.1-pro-low":                 1036,
	"gemini-2.5-pro":                     246,
	"gemini-2.5-flash":                   312,
	"gemini-3.5-flash":                   1020,
	"gemini-3.6-flash":                   1196,
	"gemini-3.7-flash-high":              1298,
	"gemini-3.7-flash-medium":            1299,
	"gemini-3.7-flash-low":               1300,
	"gemini-3.7-flash-tiered":            1301,
	"gemini-3.7-flash":                   1299,
	"gemini-3.8-flash-high":              1318,
	"gemini-3.8-flash-medium":            1319,
	"gemini-3.8-flash-low":               1320,
	"gemini-3.8-flash":                   1319,
	"gemini-4-argon-high":                1417,
	"gemini-4-argon-medium":              1418,
	"gemini-4-argon-low":                 1419,
	"gemini-4-argon":                     1418,
	"gemini-4-carbon-high":               1427,
	"gemini-4-carbon-medium":             1437,
	"gemini-4-carbon-low":                1438,
	"gemini-4-carbon":                    1437,
	"gemini-4-hydrogen-xhigh":            1464,
	"gemini-4-hydrogen-high":             1461,
	"gemini-4-hydrogen-medium":           1462,
	"gemini-4-hydrogen-low":              1463,
	"gemini-4-hydrogen":                  1462,
	"gemini-3.1-pro-preview-customtools": 1037,
	"gemini-3.1-flash-lite":              1050,
	"gemini-3.5-flash-lite":              1198,
	"gemini-3-flash":                     1018,
	"gemini-3-flash-preview":             1018,
	"gemini-3-pro-preview":               348,
	"gemini-for-google-3.1-flash":        1059,
}

// wireAliases is geminimodels' map.init.0 (25 keys): the id agy puts in the
// gateway URL instead of the listed one. It drops the effort suffix; for an id
// without built-in metadata the effort then comes only from thinkingBudget.
var wireAliases = map[string]string{
	"gemini-3.1-pro-low":       "gemini-3.1-pro-preview",
	"gemini-3.1-pro-high":      "gemini-3.1-pro-preview",
	"gemini-3.6-flash-high":    "gemini-3.6-flash",
	"gemini-3.6-flash-medium":  "gemini-3.6-flash",
	"gemini-3.6-flash-low":     "gemini-3.6-flash",
	"gemini-3.7-flash-high":    "gemini-3.7-flash",
	"gemini-3.7-flash-medium":  "gemini-3.7-flash",
	"gemini-3.7-flash-low":     "gemini-3.7-flash",
	"gemini-3.8-flash-high":    "gemini-3.8-flash",
	"gemini-3.8-flash-medium":  "gemini-3.8-flash",
	"gemini-3.8-flash-low":     "gemini-3.8-flash",
	"gemini-4-argon-high":      "gemini-4-argon",
	"gemini-4-argon-medium":    "gemini-4-argon",
	"gemini-4-argon-low":       "gemini-4-argon",
	"gemini-4-carbon-high":     "gemini-4-carbon",
	"gemini-4-carbon-medium":   "gemini-4-carbon",
	"gemini-4-carbon-low":      "gemini-4-carbon",
	"gemini-4-hydrogen-xhigh":  "gemini-4-hydrogen",
	"gemini-4-hydrogen-high":   "gemini-4-hydrogen",
	"gemini-4-hydrogen-medium": "gemini-4-hydrogen",
	"gemini-4-hydrogen-low":    "gemini-4-hydrogen",
	"gemini-4-neon-xhigh":      "gemini-4-neon",
	"gemini-4-neon-high":       "gemini-4-neon",
	"gemini-4-neon-medium":     "gemini-4-neon",
	"gemini-4-neon-low":        "gemini-4-neon",
}

// infoTable is geminimodels' model-info slice (86 entries) in order: each
// entry's Model enum and name. Entry 0 (enum 0, "unspecified") and nil
// entries are left out. Entries marked "init" are filled at start-up by
// defaultmodels.Gemini36Flash and its siblings; their enums come from those
// functions' level switches.
var infoTable = []struct {
	enum int
	name string
}{
	{235, "chat_20706"},
	{269, "chat_23310"},
	{312, "gemini-2.5-flash"},
	{1018, "gemini-3-flash-preview"},
	{1036, "gemini-3.1-pro-low-thinking"},
	{1037, "gemini-api:/models/snowball"},
	{313, "gemini-2.5-flash-1p-thoughts"},
	{329, "gemini-2.5-flash-1p-thoughts"},
	{330, "gemini-2.5-flash-lite"},
	{246, "gemini-2.5-pro-windsurf"},
	{331, "gemini-2.5-pro-windsurf-debug"},
	{327, "gemini-for-google-2p5-pro"},
	{328, "nemosreef"},
	{336, "horizondawn"},
	{337, "pureprism"},
	{338, "gentleisland"},
	{343, "orionfire"},
	{347, "cosmicforge"},
	{352, "gemini-3-pro-preview"},
	{353, "gemini-3-pro-preview"},
	{348, "gemini-3-pro-preview"},
	{350, "infinityjet"},
	{351, "infinitybloom"},
	{323, "policy://gemini_default"},
	{354, "policy://gemax_sampler"},
	{326, ""},
	{344, "evergreen://blade:gdm-aip-fastpath-agent-generate-service-prod/lmroot-v3-text-uniserve:/gdm-jetski/jetski-tab-v3xs"},
	{345, "evergreen://blade:gdm-aip-fastpath-agent-generate-service-prod/lmroot-v3-text-uniserve:/gdm-jetski/jetski-tab-jump-v3xs"},
	{332, "gemini-2.5-flash-image-preview"},
	{1021, "gemini-3.1-flash-image"},
	{335, "rev19-uic3-1p"},
	{339, "rainsong"},
	{346, "proxy-jarvis"},
	{349, "orionfire;/aistudio/gemini-v4s-jarvis-sc"},
	{1050, "gemini-3.1-flash-lite-preview"},
	{1009, "gemini-3-pro-image"},
	{1149, "gemini-3.5-flash"},
	{1020, "gemini-3.5-flash-low"},
	{1187, "gemini-3.5-flash-extra-low"},
	{1071, "gemini-3.6-flash-high"},   // init
	{1072, "gemini-3.6-flash-medium"}, // init
	{1073, "gemini-3.6-flash-low"},    // init
	{1196, "gemini-3.6-flash"},
	{1300, "gemini-3.7-flash-low"},     // init
	{1299, "gemini-3.7-flash-medium"},  // init
	{1298, "gemini-3.7-flash-high"},    // init
	{1320, "gemini-3.8-flash-low"},     // init
	{1319, "gemini-3.8-flash-medium"},  // init
	{1318, "gemini-3.8-flash-high"},    // init
	{1419, "gemini-4-argon-low"},       // init
	{1418, "gemini-4-argon-medium"},    // init
	{1417, "gemini-4-argon-high"},      // init
	{1438, "gemini-4-carbon-low"},      // init
	{1437, "gemini-4-carbon-medium"},   // init
	{1427, "gemini-4-carbon-high"},     // init
	{1463, "gemini-4-hydrogen-low"},    // init
	{1462, "gemini-4-hydrogen-medium"}, // init
	{1461, "gemini-4-hydrogen-high"},   // init
	{1464, "gemini-4-hydrogen-xhigh"},  // init
	{1059, "gemini-for-google-3.1-flash"},
	{281, "claude-sonnet-4@20250514"},
	{282, "claude-sonnet-4@20250514"},
	{333, "claude-sonnet-4-5@20250929"},
	{334, "claude-sonnet-4-5@20250929"},
	{340, "claude-haiku-4-5@20251001"},
	{341, "claude-haiku-4-5@20251001"},
	{290, "claude-opus-4@20250514"},
	{291, "claude-opus-4@20250514"},
	{1011, "claude-opus-4-5@20251101"},
	{1012, "claude-opus-4-5@20251101"},
	{1026, "claude-opus-4-6@default"},
	{1035, "claude-sonnet-4-6@default"},
	{1188, "ark-v4xs"},
	{1254, "claude-opus-4-8@default"},
	{1368, "claude-opus-5-5@default"},
	{1377, "claude-sonnet-5-5@default"},
	{1400, "claude-opus-5-5-low"},
	{1401, "claude-opus-5-5-medium"},
	{1402, "claude-opus-5-5-high"},
	{1406, "claude-opus-5-5-max"},
	{1403, "claude-sonnet-5-5-low"},
	{1404, "claude-sonnet-5-5-medium"},
	{1405, "claude-sonnet-5-5-high"},
	{1407, "claude-sonnet-5-5-max"},
	{1250, "glm-5.2-fp8"},
}

const (
	// PlaceholderSlots is how many ids without built-in metadata agy keeps.
	PlaceholderSlots = 32
	// placeholderBase is MODEL_PLACEHOLDER_M334's enum (1000 + 334).
	placeholderBase = 1334
)

// imageEnums are the built-in enums agy treats as image models.
var imageEnums = map[int]bool{1021: true, 1009: true, 332: true}

var (
	infoEnums = map[int]bool{}
	infoNames = map[string]int{}
)

func init() {
	for _, entry := range infoTable {
		infoEnums[entry.enum] = true
		if _, ok := infoNames[entry.name]; !ok && entry.name != "" {
			infoNames[entry.name] = entry.enum
		}
	}
}

// BuiltinEnum returns agy's built-in Model enum for id and whether agy has
// built-in metadata for it (LookupModelInfo), which needs no placeholder slot
// unless an earlier entry took the same enum.
func BuiltinEnum(id string) (int, bool) {
	id = strings.TrimPrefix(id, "models/")
	candidates := []string{id}
	if base, ok := strings.CutSuffix(id, "-preview"); ok {
		candidates = append(candidates, base)
	} else {
		candidates = append(candidates, id+"-preview")
	}
	for _, candidate := range candidates {
		if enum, ok := builtinIDEnum[candidate]; ok && infoEnums[enum] {
			return enum, true
		}
	}
	for _, candidate := range candidates {
		if enum, ok := infoNames[candidate]; ok {
			return enum, true
		}
	}
	return 0, false
}

// HiddenAsImage reports whether agy drops id as an image model
// (codeassistclient.IsGatewayImageModel, or a built-in image enum).
func HiddenAsImage(id string) bool {
	s := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(id), "models/"), "-preview")
	if strings.HasPrefix(s, "imagen") || strings.HasPrefix(s, "dall-e") || strings.HasSuffix(s, "-image") ||
		strings.HasSuffix(s, "_image") || strings.Contains(s, "image-gen") || strings.Contains(s, "image-generation") {
		return true
	}
	enum, ok := BuiltinEnum(id)
	return ok && imageEnums[enum]
}

// slotHash is the placeholder slot agy tries first for id.
func slotHash(id string) int {
	return int(crc32.ChecksumIEEE([]byte(id)) & (PlaceholderSlots - 1))
}

// WireID is the model id agy sends to the gateway for id.
func WireID(id string) string {
	if alias, ok := wireAliases[id]; ok {
		return alias
	}
	return id
}
