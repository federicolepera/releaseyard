package dashboard

import (
	"encoding/json"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	changeHistoryAnnotation = "platform.releaseyard.io/change-history"
	maxChangeHistoryEntries = 10
)

// changeHistoryEntry is one recorded configuration change of an Application.
// History is stored newest-first in an annotation so it travels with the object.
type changeHistoryEntry struct {
	Version int64  `json:"version"`
	At      string `json:"at"`
	By      string `json:"by,omitempty"`
	Summary string `json:"summary,omitempty"`
}

func decodeChangeHistory(app *platformv1alpha1.Application) []changeHistoryEntry {
	raw := app.GetAnnotations()[changeHistoryAnnotation]
	if raw == "" {
		return nil
	}
	var entries []changeHistoryEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	return entries
}

// recordChangeHistory prepends an entry for the upcoming generation. Call it
// before the Create/Update so the annotation is persisted in the same request.
func recordChangeHistory(app *platformv1alpha1.Application, by, summary string) {
	entries := decodeChangeHistory(app)
	entry := changeHistoryEntry{
		Version: app.Generation + 1,
		At:      time.Now().UTC().Format(time.RFC3339),
		By:      by,
		Summary: summary,
	}
	updated := append([]changeHistoryEntry{entry}, entries...)
	if len(updated) > maxChangeHistoryEntries {
		updated = updated[:maxChangeHistoryEntries]
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return
	}
	annotations := app.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[changeHistoryAnnotation] = string(encoded)
	app.SetAnnotations(annotations)
}
