// Package photokit understands the sidecar the iOS app sends after an upload:
// what the photo library knows about an asset that the asset's own bytes do
// not.
//
// It exists for the same reason internal/takeout does, and is deliberately its
// twin. Both are pure parsing of somebody else's JSON, both are stored verbatim
// beside what they were reduced to, and both describe things that cannot be
// recovered from the original by any amount of re-reading — a heart, an album,
// and the fact that a person considered this a screenshot rather than a
// photograph.
//
// It is far smaller than takeout because the phone is the one source that can
// be asked to change its mind: the shape here is ours, so it is exactly what
// PhotoKit answers and nothing has to be reverse-engineered.
package photokit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Sidecar is what the app posts. Every field is optional: an asset with none of
// them is not described at all, so anything that arrives here said something.
type Sidecar struct {
	Favorite bool `json:"favorite"`
	// Subtypes are PhotoKit's own names — "screenshot", "livePhoto",
	// "panorama", "hdr", "timelapse", "videoCinematic". Carried as the strings
	// Apple uses rather than mapped onto names of ours, because the mapping
	// would be a guess and the strings are the fact.
	Subtypes []string `json:"subtypes"`
	// Location is PhotoKit's copy of where the photo was taken. It matters for
	// the originals that carry none of their own — anything a messaging app
	// rewrote — and is outranked by the file everywhere else. The phone sends
	// the whole CLLocation; the two coordinates are the part this reduces to a
	// column, and the altitude and accuracies ride along in the raw sidecar.
	Location *Location `json:"location"`
	// Hidden is iOS's Hidden album. The phone omits it rather than sending
	// false when it could not ask, so absent and "not hidden" read the same
	// here and differ only in the sidecar kept beside this.
	Hidden bool `json:"hidden"`
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Coordinates returns the position, or nils when there is none.
//
// Exactly zero in both axes is read as absence, the same rule takeout applies
// to Google's zeroed geoData: it is a point in the Gulf of Guinea that no
// photograph in this archive was taken at, and a serializer writing zeroes for
// "unknown" is far likelier than the alternative.
func (l *Location) Coordinates() (lat, lon *float64) {
	if l == nil || (l.Latitude == 0 && l.Longitude == 0) {
		return nil, nil
	}
	return &l.Latitude, &l.Longitude
}

// Metadata is a sidecar reduced to what the archive keeps.
//
// Most of what the phone now sends is not in here and is not meant to be: the
// burst identifier, the source type, the resource inventory and the rest of the
// fix are carried whole in Raw, where a reindex can reach them the day somebody
// works out what to do with them. Modelling a field is a claim that the archive
// acts on it, and today it acts on four.
type Metadata struct {
	Raw json.RawMessage

	Favorite bool
	Hidden   bool
	Subtypes []string
	GPSLat   *float64
	GPSLon   *float64
}

// Normalize reads one sidecar.
func Normalize(raw []byte) (Metadata, error) {
	var s Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return Metadata{}, fmt.Errorf("parse photokit sidecar: %w", err)
	}

	m := Metadata{Raw: json.RawMessage(raw), Favorite: s.Favorite, Hidden: s.Hidden}
	m.GPSLat, m.GPSLon = s.Location.Coordinates()
	for _, subtype := range s.Subtypes {
		if name := strings.TrimSpace(subtype); name != "" {
			m.Subtypes = append(m.Subtypes, name)
		}
	}
	return m, nil
}
