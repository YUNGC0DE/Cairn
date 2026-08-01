package transcript

import "time"

// Window narrows a session to the messages that happened inside (from, to].
// A zero from means "since the beginning", a zero to means "until now".
//
// This is what makes retrospective analysis possible: a session spans many
// commits, so `cairn audit` reconstructs what each commit's slice of the
// conversation looked like by cutting on commit timestamps.
//
// Messages without a timestamp (agents that do not record one per turn) are kept
// only when the session as a whole falls inside the window — the best available
// approximation, and the caller is told about it via Approximate.
func (s *Session) Window(from, to time.Time) *Session {
	out := *s
	out.Messages = nil
	sessionInWindow := inWindow(s.Updated, from, to)
	approximate := false
	for _, m := range s.Messages {
		if m.Time.IsZero() {
			if sessionInWindow {
				approximate = true
				out.Messages = append(out.Messages, m)
			}
			continue
		}
		if inWindow(m.Time, from, to) {
			out.Messages = append(out.Messages, m)
		}
	}
	out.Approximate = approximate
	return &out
}

func inWindow(t, from, to time.Time) bool {
	if t.IsZero() {
		return false
	}
	if !from.IsZero() && !t.After(from) {
		return false
	}
	if !to.IsZero() && t.After(to) {
		return false
	}
	return true
}

// Span returns the first and last timestamped message times in a session.
func (s *Session) Span() (first, last time.Time) {
	for _, m := range s.Messages {
		if m.Time.IsZero() {
			continue
		}
		if first.IsZero() || m.Time.Before(first) {
			first = m.Time
		}
		if last.IsZero() || m.Time.After(last) {
			last = m.Time
		}
	}
	return first, last
}
