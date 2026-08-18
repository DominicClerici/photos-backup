package db

import (
	"context"
	"fmt"
)

// ResetLibrary empties everything that describes archived content, leaving
// pairing untouched.
//
// The split is the whole point. assets, device_assets and jobs are the index of
// what has been stored; devices and pairing_codes are credentials. A phone whose
// token survives keeps uploading against an empty archive without being paired
// again, which is what makes this recoverable from the app's side rather than
// from the keychain.
//
// One truncate is enough because the references run one way: device_assets and
// jobs both point at assets, nothing points at them, and 0004_devices.sql
// deliberately gives assets.device_id no foreign key to devices. cascade here
// therefore cannot reach a credential row — a constraint that exists so reindex
// can replay the manifest for a device that is long gone, and that pays off
// again here.
func (s *Store) ResetLibrary(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`truncate table assets, device_assets, jobs restart identity cascade`)
	if err != nil {
		return fmt.Errorf("reset library tables: %w", err)
	}
	return nil
}

// CountDevices counts paired devices, revoked ones included.
//
// Reset calls this on both sides of the truncate and refuses to report success
// if the number moved. Cheap, and it turns "cascade cannot reach devices" from a
// claim in a comment into something the command actually checks.
func (s *Store) CountDevices(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `select count(*) from devices`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count devices: %w", err)
	}
	return n, nil
}
