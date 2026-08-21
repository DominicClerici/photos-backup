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
	// PhotoKit is everything the phone read straight off PHAsset, nested as the
	// app sends it. Almost all of it stays in Raw and is not modelled; the one
	// field pulled out of it is the contributor, for the reason below.
	PhotoKit *Facts `json:"photoKit"`
}

// Facts is the nested block, reduced to the part this package reads.
//
// Everything else PHAsset answered — the burst, the resource inventory, the
// playback style — is deliberately absent and lives on in Raw. Adding a field
// here is a claim that the archive does something with it.
type Facts struct {
	Contributor *Contributor `json:"contributor"`
}

// Contributor is the person who put a photograph into an iCloud Shared Album.
//
// It is the only provenance a shared photograph has. The file carries nothing:
// Apple re-encodes what goes into a shared album, so there is no maker note and
// no owner field to read, and the name exists on the phone and nowhere else —
// until the album is left, after which it exists nowhere. That is what earns it
// a modelled field where the burst identifier does not.
//
// DisplayName is the phone's own choice of what to call them: a name where there
// was one, an address where there was not. The rest is kept for the same reason
// the sidecar is kept whole — so that a later idea about people, or a merge with
// the face tags, has something to work from.
type Contributor struct {
	DisplayName string `json:"displayName"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Email       string `json:"email"`
	// PersonID is a hash. It tells two contributors apart and identifies
	// neither, and it is never a thing to put on a screen.
	PersonID string `json:"personId"`
}

// Name is who to say added the photograph, or empty where nobody is named.
func (c *Contributor) Name() string {
	if c == nil {
		return ""
	}
	if name := strings.TrimSpace(c.DisplayName); name != "" {
		return name
	}
	full := strings.TrimSpace(strings.TrimSpace(c.FirstName) + " " + strings.TrimSpace(c.LastName))
	if full != "" {
		return full
	}
	return strings.TrimSpace(c.Email)
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
	// Contributor is who added this to a shared album, and empty for everything
	// that came off the phone's own camera. See the type.
	Contributor string
}

// Normalize reads one sidecar.
func Normalize(raw []byte) (Metadata, error) {
	var s Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return Metadata{}, fmt.Errorf("parse photokit sidecar: %w", err)
	}

	m := Metadata{Raw: json.RawMessage(raw), Favorite: s.Favorite, Hidden: s.Hidden}
	m.GPSLat, m.GPSLon = s.Location.Coordinates()
	if s.PhotoKit != nil {
		m.Contributor = s.PhotoKit.Contributor.Name()
	}
	for _, subtype := range s.Subtypes {
		if name := strings.TrimSpace(subtype); name != "" {
			m.Subtypes = append(m.Subtypes, name)
		}
	}
	return m, nil
}
