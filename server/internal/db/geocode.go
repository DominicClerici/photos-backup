package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GeocodeTarget is one asset waiting to be turned into a place name.
type GeocodeTarget struct {
	AssetID  string
	Lat, Lon float64
}

// AssetPlace pairs an asset with what the geocoder made of its coordinates.
type AssetPlace struct {
	AssetID string
	Place   Place
}

// PendingGeocode lists assets carrying a GPS fix that nobody has resolved yet,
// or every one of them when all is set — which is what a better extract or a
// changed rule needs.
//
// The vault is excluded, and not as an optimisation. A place name is a
// description of where a photograph was taken, and it is a considerably more
// legible one than a pair of coordinates: writing "Chicago" onto a hidden
// photograph's row would be this server recording in plain text the thing the
// vault exists to stop it knowing. The scrub empties the coordinates on the way
// in, so in practice there is nothing here to resolve either — this predicate
// is what makes that true by intent rather than by accident.
func (s *Store) PendingGeocode(ctx context.Context, all bool) ([]GeocodeTarget, error) {
	const query = `
		select id::text, gps_lat, gps_lon
		from assets
		where vault = '' and gps_lat is not null and gps_lon is not null
		  and ($1 or geocoded_at is null)
		order by id`

	rows, err := s.pool.Query(ctx, query, all)
	if err != nil {
		return nil, fmt.Errorf("list assets to geocode: %w", err)
	}
	defer rows.Close()

	var targets []GeocodeTarget
	for rows.Next() {
		var t GeocodeTarget
		if err := rows.Scan(&t.AssetID, &t.Lat, &t.Lon); err != nil {
			return nil, fmt.Errorf("scan asset to geocode: %w", err)
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// AssetToGeocode reads the coordinates one asset should be resolved from, and
// reports false for the majority that have none.
//
// It exists so the metadata job asks the same question the backfill asks,
// against the same row, instead of reasoning about what it just wrote. The
// coordinates it wants may have come from an import sidecar rather than from
// the file — ApplyMetadata coalesces the two, and on a first run the sidecar's
// value is only reachable after that update has run.
//
// Unlike PendingGeocode this ignores geocoded_at. A metadata job re-running
// means the file has been read again, and if it now says somewhere else then
// the place name derived from the old coordinates is simply wrong.
func (s *Store) AssetToGeocode(ctx context.Context, assetID string) (GeocodeTarget, bool, error) {
	const query = `
		select id::text, gps_lat, gps_lon
		from assets
		where id = $1::uuid and vault = ''
		  and gps_lat is not null and gps_lon is not null`

	var t GeocodeTarget
	err := s.pool.QueryRow(ctx, query, assetID).Scan(&t.AssetID, &t.Lat, &t.Lon)
	if errors.Is(err, pgx.ErrNoRows) {
		return GeocodeTarget{}, false, nil
	}
	if err != nil {
		return GeocodeTarget{}, false, fmt.Errorf("read coordinates of %s: %w", assetID, err)
	}
	return t, true, nil
}

// ApplyPlaces records what the geocoder resolved for a batch of assets.
//
// An empty Place is a result rather than a failure and is stored as one: the
// row gets null names and a geocoded_at, which says somebody looked and there
// was nothing within reach. Without that distinction every photograph taken
// over open water would come back as pending on every run of the backfill,
// forever.
//
// The vault check is repeated here even though PendingGeocode already excluded
// it, because the two are separated by however long the resolve took and a
// photograph can be hidden in between. It is the same guard the worker's
// vaulted() applies for the same reason.
func (s *Store) ApplyPlaces(ctx context.Context, places []AssetPlace) (int64, error) {
	if len(places) == 0 {
		return 0, nil
	}

	ids := make([]string, len(places))
	cities := make([]string, len(places))
	admins := make([]string, len(places))
	countries := make([]string, len(places))
	sources := make([]string, len(places))
	for i, p := range places {
		ids[i] = p.AssetID
		cities[i] = p.Place.City
		admins[i] = p.Place.Admin1
		countries[i] = p.Place.Country
		sources[i] = p.Place.Source
	}

	const update = `
		update assets a set
			place_city    = nullif(v.city, ''),
			place_admin1  = nullif(v.admin1, ''),
			place_country = nullif(v.country, ''),
			place_source  = nullif(v.source, ''),
			geocoded_at   = now()
		from unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[])
		     as v (id, city, admin1, country, source)
		where a.id = v.id and a.vault = ''`

	tag, err := s.pool.Exec(ctx, update, ids, cities, admins, countries, sources)
	if err != nil {
		return 0, fmt.Errorf("apply places: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ApplyPlace is the single-asset form, for the metadata job.
func (s *Store) ApplyPlace(ctx context.Context, assetID string, p Place) error {
	_, err := s.ApplyPlaces(ctx, []AssetPlace{{AssetID: assetID, Place: p}})
	return err
}
