package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

const (
	// maxCheckItems bounds one request. The app batches at 200; the cap is here
	// so a confused client cannot ask the server to build million-element arrays.
	maxCheckItems = 500
	// maxCheckBody is generous for 500 items of metadata and small enough that a
	// runaway client cannot buffer the process to death.
	maxCheckBody = 1 << 20

	statusHave    = "have"
	statusUnknown = "unknown"
	statusWant    = "want"
)

type syncCheckRequest struct {
	DeviceID string          `json:"deviceId"`
	Items    []syncCheckItem `json:"items"`
}

// syncCheckItem is one local asset the phone is asking about. MD5 and Size are
// absent in the first round, and that absence is the whole design: the phone
// must not have to hash 100GB to discover the server already holds all of it.
type syncCheckItem struct {
	LocalID    string     `json:"localId"`
	MD5        string     `json:"md5"`
	Size       *int64     `json:"size"`
	ModifiedAt *time.Time `json:"modifiedAt"`
}

type syncCheckResult struct {
	LocalID string `json:"localId"`
	Status  string `json:"status"`
	AssetID string `json:"assetId,omitempty"`
}

type syncCheckResponse struct {
	Results []syncCheckResult `json:"results"`
}

func (s *Server) handleSyncCheck(w http.ResponseWriter, r *http.Request) {
	var req syncCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCheckBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	if err := validateCheckRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeJSON(w, http.StatusOK, syncCheckResponse{Results: []syncCheckResult{}})
		return
	}

	results, err := s.checkItems(r.Context(), req.DeviceID, req.Items)
	if err != nil {
		s.logger().Error("sync check", "error", err, "device_id", req.DeviceID)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, syncCheckResponse{Results: results})
}

func validateCheckRequest(req syncCheckRequest) error {
	if strings.TrimSpace(req.DeviceID) == "" {
		return fmt.Errorf("deviceId is required")
	}
	if len(req.Items) > maxCheckItems {
		return fmt.Errorf("%d items exceeds the %d-item limit", len(req.Items), maxCheckItems)
	}
	for i, item := range req.Items {
		if strings.TrimSpace(item.LocalID) == "" {
			return fmt.Errorf("items[%d].localId is required", i)
		}
		// A digest without a length is not a content claim this server will act
		// on, and accepting it silently would let the phone loop: it would keep
		// being told "unknown" for an item it has already hashed.
		if item.MD5 != "" && item.Size == nil {
			return fmt.Errorf("items[%d] supplied md5 without size", i)
		}
		if item.Size != nil && *item.Size < 0 {
			return fmt.Errorf("items[%d].size is negative", i)
		}
	}
	return nil
}

// checkItems answers have/unknown/want for each item, in the order asked:
//
//  1. the (device, local id) mapping already resolves, and the modification
//     time still agrees                                     -> have
//  2. no digest was supplied, so nothing more can be said    -> unknown
//  3. exactly one archived asset matches (md5, byte size)    -> have, recording
//     the mapping so the next run answers at step 1
//  4. anything else, ambiguous content matches included      -> want
//
// Step 3 is the only place the server claims to hold content it has not hashed
// on this request, so it is deliberately narrow. An md5 and size that match two
// different assets identify nothing, and fall through to want: the bytes come
// over the wire and the real sha256 settles it.
func (s *Server) checkItems(ctx context.Context, deviceID string, items []syncCheckItem) ([]syncCheckResult, error) {
	// Normalized to the same precision the upload path stores, or the equality
	// this lookup depends on could never hold. See normalizeTime.
	refs := make([]db.LocalRef, len(items))
	for i, item := range items {
		refs[i] = db.LocalRef{LocalID: item.LocalID, ModifiedAt: normalizeTime(item.ModifiedAt)}
	}
	known, err := s.Store.KnownMappings(ctx, deviceID, refs)
	if err != nil {
		return nil, err
	}

	// Deduplicated, because two local ids carrying identical bytes is the
	// ordinary case this table exists to handle.
	var keys []db.ContentKey
	asked := make(map[db.ContentKey]bool)
	for _, item := range items {
		if _, ok := known[item.LocalID]; ok {
			continue
		}
		key, ok := contentKey(item)
		if !ok || asked[key] {
			continue
		}
		asked[key] = true
		keys = append(keys, key)
	}

	matches, err := s.Store.AssetsByContent(ctx, keys)
	if err != nil {
		return nil, err
	}

	results := make([]syncCheckResult, len(items))
	var learned []db.Mapping
	recorded := make(map[string]bool, len(items))
	for i, item := range items {
		result := syncCheckResult{LocalID: item.LocalID}

		switch key, hasKey := contentKey(item); {
		case known[item.LocalID] != "":
			result.Status = statusHave
			result.AssetID = known[item.LocalID]
		case !hasKey:
			result.Status = statusUnknown
		case matches[key].Matches == 1:
			result.Status = statusHave
			result.AssetID = matches[key].AssetID
			// One statement cannot upsert the same key twice, and a batch is
			// allowed to repeat a local id.
			if !recorded[item.LocalID] {
				recorded[item.LocalID] = true
				learned = append(learned, db.Mapping{
					LocalID:    item.LocalID,
					AssetID:    matches[key].AssetID,
					ModifiedAt: normalizeTime(item.ModifiedAt),
				})
			}
		default:
			result.Status = statusWant
		}

		results[i] = result
	}

	if err := s.Store.RecordMappings(ctx, deviceID, learned); err != nil {
		return nil, err
	}
	return results, nil
}

// contentKey reports the content key an item declares, if it declared one.
func contentKey(item syncCheckItem) (db.ContentKey, bool) {
	sum := strings.ToLower(strings.TrimSpace(item.MD5))
	if sum == "" || item.Size == nil {
		return db.ContentKey{}, false
	}
	return db.ContentKey{MD5: sum, ByteSize: *item.Size}, true
}
