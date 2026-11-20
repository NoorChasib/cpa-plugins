package plugin

import "testing"

// CPA's Plugins page shows the name a plugin registers with, so it is the
// catalog's display name; the ID keeps keying the configuration and routes.
func TestRegistersItsDisplayName(t *testing.T) {
	if name := registration(rawManagementSchema).Metadata.Name; name != "Quota Glance" {
		t.Fatalf("registered name = %q, want %q", name, "Quota Glance")
	}
	if ID != "quota-glance" {
		t.Fatalf("ID = %q; it keys the configuration and must not change", ID)
	}
}
