package api

import (
	"net/http"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// What the archive thinks one photograph is, read back for the person looking
// at it.
//
// The mirror of /v1/search: that endpoint takes a sentence and ranks
// photographs, and this takes a photograph and hands back the words the ranking
// was built out of — the caption, the vocabulary, the recognised text, and
// whether the encoder has looked at it at all.
//
// Its own route rather than more fields on /v1/assets/{id}, for two reasons
// that point the same way. The detail load happens on every arrow-key press
// through the viewer and this does not: the panel is a toggle, and somebody
// stepping through a hundred photographs with it shut should not be carrying a
// hundred pages of recognised text they never asked to see. And an OCR blob is
// unbounded where every other field on assetDetail is a scalar — a screenshot
// of a terminal is kilobytes of text, which is worth fetching when a panel is
// open and is not worth adding to the timeline's critical path.
func (s *Server) handleAssetAnalysis(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	// Nothing has been written about a hidden photograph — the vault guard is
	// on all three write paths — so this is not hiding an answer so much as
	// declining to go and confirm there isn't one. See db.AssetAnalysis.
	if asset.Vault != "" {
		writeJSON(w, http.StatusOK, db.Analysis{})
		return
	}

	analysis, err := s.Store.AssetAnalysis(r.Context(), asset.ID)
	if err != nil {
		// Not fatal, and the panel is built to draw a partial answer: half of
		// what a model said is worth more than an error where the caption was.
		s.logger().Warn("load asset analysis", "error", err, "asset", asset.ID)
	}
	writeJSON(w, http.StatusOK, analysis)
}
