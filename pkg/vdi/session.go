package vdi

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Clock makes session policy deterministic in tests and avoids coupling
// reclaim decisions to wall-clock sleeps.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Session describes console presence independently from pool ownership.
type Session struct {
	Member         string    `json:"member"`
	Identity       string    `json:"identity"`
	Connected      bool      `json:"connected"`
	ConnectedAt    time.Time `json:"connectedAt"`
	DisconnectedAt time.Time `json:"disconnectedAt,omitempty"`
	MaxUntil       time.Time `json:"maxUntil,omitempty"`
	Ephemeral      bool      `json:"ephemeral"`
	reclaiming     bool
}

// AuditEvent records policy decisions and lifecycle outcomes. It is intended
// for an embedding broker to persist or forward to its audit sink.
type AuditEvent struct {
	At       time.Time `json:"at"`
	Member   string    `json:"member"`
	Action   string    `json:"action"`
	Identity string    `json:"identity,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Reclaimer performs the backend-specific reclaim. Persistent members should
// stop and retain disks; ephemeral members should be recreated by the caller.
type Reclaimer func(member string, ephemeral bool) error

// SessionManager tracks console presence for already-claimed members. It is
// deliberately in-memory: a broker can reconstruct it from its persisted
// session records, and a restart must not infer abandonment from absence.
type SessionManager struct {
	mu       sync.Mutex
	clock    Clock
	grace    time.Duration
	maximum  time.Duration
	sessions map[string]Session
	audit    []AuditEvent
}

// NewSessionManager creates a conservative policy manager. A zero grace
// duration reclaims on the next Reconcile after disconnect. A zero maximum
// disables the maximum-duration policy.
func NewSessionManager(clock Clock, disconnectGrace, maximumDuration time.Duration) *SessionManager {
	if clock == nil {
		clock = realClock{}
	}
	return &SessionManager{clock: clock, grace: disconnectGrace, maximum: maximumDuration, sessions: make(map[string]Session)}
}

// Connect records presence. Reconnecting the owner cancels pending grace
// reclaim; duplicate connect events are harmless.
func (m *SessionManager) Connect(member, identity string, ephemeral bool) error {
	if member == "" || identity == "" {
		return fmt.Errorf("member and identity are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now().UTC()
	if current, ok := m.sessions[member]; ok && current.Identity != identity {
		return fmt.Errorf("member %s has an active session for %q", member, current.Identity)
	}
	current := m.sessions[member]
	if current.Member == "" {
		current = Session{Member: member, Identity: identity, ConnectedAt: now, Ephemeral: ephemeral}
		if m.maximum > 0 {
			current.MaxUntil = now.Add(m.maximum)
		}
	} else {
		current.Connected = true
		current.DisconnectedAt = time.Time{}
	}
	current.Connected = true
	m.sessions[member] = current
	m.record(now, member, "connect", identity, "", "")
	return nil
}

// Disconnect records console loss but does not reclaim until Reconcile sees
// the grace period expire.
func (m *SessionManager) Disconnect(member, identity string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.owner(member, identity)
	if err != nil {
		return err
	}
	if !s.Connected {
		return nil
	}
	s.Connected = false
	s.DisconnectedAt = m.clock.Now().UTC()
	m.sessions[member] = s
	m.record(s.DisconnectedAt, member, "disconnect", identity, "grace timer started", "")
	return nil
}

// Release immediately reclaims a session after verifying ownership.
func (m *SessionManager) Release(member, identity string, reclaim Reclaimer) error {
	return m.reclaim(member, identity, "explicit release", reclaim)
}

// ForceRelease is the administrator path and records the action separately.
func (m *SessionManager) ForceRelease(member, administrator string, reclaim Reclaimer) error {
	if administrator == "" {
		return fmt.Errorf("administrator identity is required")
	}
	m.mu.Lock()
	s, ok := m.sessions[member]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no session for member %s", member)
	}
	return m.reclaim(member, s.Identity, "administrator force-release by "+administrator, reclaim)
}

// Reconcile reclaims disconnected sessions past grace and all sessions past
// maximum duration. Maximum duration is checked even while connected.
func (m *SessionManager) Reconcile(reclaim Reclaimer) []AuditEvent {
	m.mu.Lock()
	members := make([]string, 0, len(m.sessions))
	now := m.clock.Now().UTC()
	for member := range m.sessions {
		members = append(members, member)
	}
	sort.Strings(members)
	m.mu.Unlock()
	for _, member := range members {
		m.mu.Lock()
		s, ok := m.sessions[member]
		due, reason := false, ""
		if ok && !s.MaxUntil.IsZero() && !now.Before(s.MaxUntil) {
			due, reason = true, "maximum session duration"
		}
		if ok && !s.Connected && !s.DisconnectedAt.IsZero() && !now.Before(s.DisconnectedAt.Add(m.grace)) {
			due, reason = true, "disconnect grace expired"
		}
		m.mu.Unlock()
		if due {
			_ = m.reclaim(member, s.Identity, reason, reclaim)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]AuditEvent(nil), m.audit...)
	m.audit = nil
	return out
}

func (m *SessionManager) reclaim(member, identity, reason string, reclaim Reclaimer) error {
	if reclaim == nil {
		return fmt.Errorf("reclaimer is required")
	}
	m.mu.Lock()
	s, err := m.owner(member, identity)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	policyReclaim := reason != "explicit release" && !strings.HasPrefix(reason, "administrator force-release")
	if s.reclaiming || (policyReclaim && !m.due(s, m.clock.Now().UTC())) {
		m.mu.Unlock()
		return nil
	}
	s.reclaiming = true
	m.sessions[member] = s
	m.mu.Unlock()
	if err := reclaim(member, s.Ephemeral); err != nil {
		m.mu.Lock()
		s.reclaiming = false
		m.sessions[member] = s
		m.record(m.clock.Now().UTC(), member, "reclaim-failed", identity, reason, err.Error())
		m.mu.Unlock()
		return fmt.Errorf("reclaim %s: %w", member, err)
	}
	m.mu.Lock()
	if policyReclaim {
		if current, ok := m.sessions[member]; ok && current.Connected {
			current.reclaiming = false
			m.sessions[member] = current
			m.record(m.clock.Now().UTC(), member, "reclaim-cancelled", identity, "session reconnected", "")
			m.mu.Unlock()
			return nil
		}
	}
	delete(m.sessions, member)
	m.record(m.clock.Now().UTC(), member, "reclaim", identity, reason, "")
	m.mu.Unlock()
	return nil
}

func (m *SessionManager) due(s Session, now time.Time) bool {
	return (!s.MaxUntil.IsZero() && !now.Before(s.MaxUntil)) || (!s.Connected && !s.DisconnectedAt.IsZero() && !now.Before(s.DisconnectedAt.Add(m.grace)))
}

func (m *SessionManager) owner(member, identity string) (Session, error) {
	s, ok := m.sessions[member]
	if !ok {
		return Session{}, fmt.Errorf("no session for member %s", member)
	}
	if s.Identity != identity {
		return Session{}, fmt.Errorf("member %s session is owned by %q, not %q", member, s.Identity, identity)
	}
	return s, nil
}

func (m *SessionManager) record(at time.Time, member, action, identity, reason, eventErr string) {
	m.audit = append(m.audit, AuditEvent{At: at, Member: member, Action: action, Identity: identity, Reason: reason, Error: eventErr})
}

// Sessions returns a snapshot suitable for persistence by an embedding broker.
func (m *SessionManager) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.reclaiming = false
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out
}

// Restore loads previously persisted sessions. It intentionally emits no
// disconnect or reclaim events: a broker restart is not evidence of a lost
// console connection.
func (m *SessionManager) Restore(sessions []Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range sessions {
		if s.Member == "" || s.Identity == "" {
			return fmt.Errorf("restored session requires member and identity")
		}
		s.reclaiming = false
		m.sessions[s.Member] = s
	}
	return nil
}

// Session returns a snapshot of the current session, if any.
func (m *SessionManager) Session(member string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[member]
	return s, ok
}
