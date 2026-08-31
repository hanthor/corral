package vdi

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestSessionReconnectCancelsGrace(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewSessionManager(clock, time.Minute, 0)
	if err := m.Connect("desk-1", "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect("desk-1", "alice"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second)
	if err := m.Connect("desk-1", "alice", false); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	var reclaimed bool
	m.Reconcile(func(string, bool) error { reclaimed = true; return nil })
	if reclaimed {
		t.Fatal("reconnected session was reclaimed")
	}
}

func TestSessionGraceAndDuplicateEvents(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewSessionManager(clock, time.Minute, 0)
	_ = m.Connect("desk-1", "alice", true)
	_ = m.Disconnect("desk-1", "alice")
	_ = m.Disconnect("desk-1", "alice")
	clock.Advance(time.Minute)
	var gotMember string
	var gotEphemeral bool
	events := m.Reconcile(func(member string, ephemeral bool) error { gotMember, gotEphemeral = member, ephemeral; return nil })
	if gotMember != "desk-1" || !gotEphemeral {
		t.Fatalf("reclaim = %q, %v", gotMember, gotEphemeral)
	}
	if len(events) != 3 || events[len(events)-1].Action != "reclaim" {
		t.Fatalf("unexpected audit events: %+v", events)
	}
	if _, ok := m.Session("desk-1"); ok {
		t.Fatal("reclaimed session still present")
	}
}

func TestSessionMaximumIsIndependentOfDisconnect(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewSessionManager(clock, 24*time.Hour, time.Hour)
	_ = m.Connect("desk-1", "alice", false)
	clock.Advance(time.Hour)
	called := false
	m.Reconcile(func(string, bool) error { called = true; return nil })
	if !called {
		t.Fatal("maximum session duration did not reclaim connected session")
	}
}

func TestSessionReleaseOwnershipAndFailureRecovery(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewSessionManager(clock, time.Minute, 0)
	_ = m.Connect("desk-1", "alice", false)
	if err := m.Release("desk-1", "bob", func(string, bool) error { return nil }); err == nil {
		t.Fatal("non-owner released session")
	}
	wantErr := errors.New("backend unavailable")
	if err := m.Release("desk-1", "alice", func(string, bool) error { return wantErr }); err == nil {
		t.Fatal("expected lifecycle failure")
	}
	if _, ok := m.Session("desk-1"); !ok {
		t.Fatal("failed reclaim lost session state")
	}
	if err := m.Release("desk-1", "alice", func(string, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRestartDoesNotInferReclaim(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewSessionManager(clock, time.Minute, 0)
	_ = m.Connect("desk-1", "alice", false)
	if events := m.Reconcile(func(string, bool) error { return nil }); len(events) != 1 || events[0].Action != "connect" {
		t.Fatalf("unexpected restart reconciliation audit: %+v", events)
	}
	if _, ok := m.Session("desk-1"); !ok {
		t.Fatal("live claim was reclaimed")
	}
}
