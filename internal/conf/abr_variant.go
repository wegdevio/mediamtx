package conf

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
)

// ABRVariant is an item of abrVariants.
// It points to another path that carries one codec / resolution of the same content.
// When the ABR path is a regular expression, Path can contain $G1, $G2...,
// replaced with the regular expression groups of the requested path.
type ABRVariant struct {
	Path    string `json:"path"`
	Codec   string `json:"codec"`
	Name    string `json:"name"`
	Bitrate int    `json:"bitrate"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *ABRVariant) UnmarshalJSON(b []byte) error {
	type alias ABRVariant
	err := jsonwrapper.Unmarshal(b, (*alias)(v))
	if err != nil {
		return err
	}

	err = IsValidPathName(reSourcePlaceholder.ReplaceAllString(v.Path, "abc"))
	if err != nil {
		return fmt.Errorf("invalid ABR variant path '%s': %w", v.Path, err)
	}

	if v.Name == "" {
		return fmt.Errorf("ABR variant '%s': name is required", v.Path)
	}

	switch v.Codec {
	case "av1", "h264":
	default:
		return fmt.Errorf("unsupported ABR variant codec '%s' (supported: av1, h264)", v.Codec)
	}

	if v.Bitrate <= 0 {
		return fmt.Errorf("ABR variant '%s': bitrate must be greater than zero", v.Path)
	}

	return nil
}

// ResolveABRVariantPath replaces $G1, $G2... in a variant path with the groups
// of the regular expression that matched the ABR path.
func ResolveABRVariantPath(p string, matches []string) string {
	for i := len(matches) - 1; i >= 1; i-- {
		p = strings.ReplaceAll(p, "$G"+strconv.Itoa(i), matches[i])
	}
	return p
}

// PickABRVariant picks a single variant of an ABR path, for protocols that
// serve one fixed layer (HTTP-FLV, MoQ). It returns the variant path and codec.
// H264 by default, highest bitrate first; ?codec=av1 selects the AV1 ladder,
// ?start=low|<variant name>|<variant path> selects the layer.
func PickABRVariant(pathConf *Path, pathName string, query string) (string, string, error) {
	q, _ := url.ParseQuery(query)

	codec := q.Get("codec")
	if codec == "" {
		codec = "h264"
		if !slices.ContainsFunc(pathConf.ABRVariants, func(v ABRVariant) bool { return v.Codec == "h264" }) {
			codec = pathConf.ABRVariants[0].Codec
		}
	}

	var matches []string
	if pathConf.Regexp != nil {
		matches = pathConf.Regexp.FindStringSubmatch(pathName)
	}

	var variants []ABRVariant
	for _, v := range pathConf.ABRVariants {
		if v.Codec == codec {
			v.Path = ResolveABRVariantPath(v.Path, matches)
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		return "", "", fmt.Errorf("no %s variant", codec)
	}
	slices.SortStableFunc(variants, func(x, y ABRVariant) int { return y.Bitrate - x.Bitrate })

	pick := variants[0]
	switch h := q.Get("start"); h {
	case "", "high":
	case "low":
		pick = variants[len(variants)-1]
	default:
		for _, v := range variants {
			if v.Name == h || v.Path == h {
				pick = v
			}
		}
	}

	return pick.Path, codec, nil
}
