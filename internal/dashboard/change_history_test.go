package dashboard

import (
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func TestRecordChangeHistoryPrependsNewestFirst(t *testing.T) {
	app := &platformv1alpha1.Application{}
	recordChangeHistory(app, "alice", "Initial configuration")
	entries := decodeChangeHistory(app)
	if len(entries) != 1 || entries[0].Version != 1 || entries[0].By != "alice" || entries[0].Summary != "Initial configuration" {
		t.Fatalf("first entry = %#v", entries)
	}
	app.Generation = 1
	recordChangeHistory(app, "bob", "Updated values")
	entries = decodeChangeHistory(app)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Version != 2 || entries[0].By != "bob" || entries[1].Version != 1 {
		t.Fatalf("entries = %#v", entries)
	}
}
