package conf

import (
	"fmt"

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
