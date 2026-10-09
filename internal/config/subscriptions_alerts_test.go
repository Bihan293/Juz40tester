package config

import "testing"

func TestAlertRecipientsNeverReachNonAdmins(t *testing.T) {
	s := Subscriptions{AdminIDs: []int64{1, 2}}
	if got := s.AlertRecipients(); len(got) != 2 {
		t.Fatalf("default = all admins, got %v", got)
	}
	s.AlertIDs = []int64{2, 99}
	if got := s.AlertRecipients(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("a non-admin id must be dropped, got %v", got)
	}
	s = Subscriptions{AlertIDs: []int64{5}}
	if got := s.AlertRecipients(); len(got) != 0 {
		t.Fatalf("no admins → nobody gets alerts, got %v", got)
	}
}
