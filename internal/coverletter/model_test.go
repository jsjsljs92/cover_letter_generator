package coverletter

import (
	"testing"
)

// transition is one row of the state machine matrix.
type transition struct {
	from Status
	to   Status
	want bool
}

// transitionMatrix spells out every (from, to) pair across all four statuses
// plus the invalid ones, so a change to allowedTransitions cannot slip through
// unnoticed in either direction.
//
// The permitted edges are exactly:
//
//	created    -> processing, failed
//	processing -> completed, failed
//	completed  -> (terminal)
//	failed     -> (terminal)
//
// Note that completed -> failed is deliberately absent: a request that already
// produced a downloadable PDF must never be retroactively marked failed.
var transitionMatrix = []transition{
	{from: StatusCreated, to: StatusCreated, want: false},
	{from: StatusCreated, to: StatusProcessing, want: true},
	{from: StatusCreated, to: StatusCompleted, want: false},
	{from: StatusCreated, to: StatusFailed, want: true},

	{from: StatusProcessing, to: StatusCreated, want: false},
	{from: StatusProcessing, to: StatusProcessing, want: false},
	{from: StatusProcessing, to: StatusCompleted, want: true},
	{from: StatusProcessing, to: StatusFailed, want: true},

	{from: StatusCompleted, to: StatusCreated, want: false},
	{from: StatusCompleted, to: StatusProcessing, want: false},
	{from: StatusCompleted, to: StatusCompleted, want: false},
	{from: StatusCompleted, to: StatusFailed, want: false},

	{from: StatusFailed, to: StatusCreated, want: false},
	{from: StatusFailed, to: StatusProcessing, want: false},
	{from: StatusFailed, to: StatusCompleted, want: false},
	{from: StatusFailed, to: StatusFailed, want: false},
}

func TestStatusIsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Status
		want bool
	}{
		{name: "created", in: StatusCreated, want: true},
		{name: "processing", in: StatusProcessing, want: true},
		{name: "completed", in: StatusCompleted, want: true},
		{name: "failed", in: StatusFailed, want: true},

		{name: "empty", in: Status(""), want: false},
		{name: "unknown word", in: Status("pending"), want: false},
		{name: "wrong case", in: Status("Created"), want: false},
		{name: "padded", in: Status(" created "), want: false},
		{name: "numeric", in: Status("1"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.in.IsValid(); got != tc.want {
				t.Errorf("Status(%q).IsValid() = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

func TestStatusIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Status
		want bool
	}{
		{name: "created is not terminal", in: StatusCreated, want: false},
		{name: "processing is not terminal", in: StatusProcessing, want: false},
		{name: "completed is terminal", in: StatusCompleted, want: true},
		{name: "failed is terminal", in: StatusFailed, want: true},

		// An unknown status is not terminal, which is why the repository treats
		// an unrecognised value as a conflict rather than a settled result.
		{name: "empty is not terminal", in: Status(""), want: false},
		{name: "unknown is not terminal", in: Status("archived"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.in.IsTerminal(); got != tc.want {
				t.Errorf("Status(%q).IsTerminal() = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanTransition(t *testing.T) {
	t.Parallel()

	for _, tc := range transitionMatrix {
		t.Run(string(tc.from)+"_to_"+string(tc.to), func(t *testing.T) {
			t.Parallel()

			if got := CanTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("CanTransition(%q, %q) = %t, want %t", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestTransitionMatrixIsExhaustive guards the table itself: if a status is
// added to the enum without a matching row, the matrix stops covering the
// state machine and the tests above would quietly pass.
func TestTransitionMatrixIsExhaustive(t *testing.T) {
	t.Parallel()

	all := AllStatuses()
	if len(all) != 4 {
		t.Fatalf("AllStatuses() returned %d statuses (%v); update transitionMatrix for the new status", len(all), all)
	}

	seen := make(map[Status]map[Status]bool, len(all))
	for _, from := range all {
		seen[from] = make(map[Status]bool, len(all))
	}
	for _, tc := range transitionMatrix {
		row, ok := seen[tc.from]
		if !ok {
			t.Errorf("transitionMatrix references unknown source status %q", tc.from)
			continue
		}
		if row[tc.to] {
			t.Errorf("transitionMatrix has a duplicate row for %q -> %q", tc.from, tc.to)
		}
		row[tc.to] = true
	}

	for _, from := range all {
		for _, to := range all {
			if !seen[from][to] {
				t.Errorf("transitionMatrix is missing the row %q -> %q", from, to)
			}
		}
	}
}

// TestTerminalStatusesHaveNoOutgoingTransitions asserts the invariant the
// workflow depends on: once a record is terminal it can never move again.
func TestTerminalStatusesHaveNoOutgoingTransitions(t *testing.T) {
	t.Parallel()

	for _, from := range AllStatuses() {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range AllStatuses() {
			if CanTransition(from, to) {
				t.Errorf("terminal status %q can transition to %q", from, to)
			}
		}
	}
}

// TestAllStatusesCoversEveryValidStatus keeps IsValid and AllStatuses in
// agreement; IsValid is implemented in terms of AllStatuses, so a status added
// to one but not the other would be an invisible contract change.
func TestAllStatusesCoversEveryValidStatus(t *testing.T) {
	t.Parallel()

	all := AllStatuses()
	for _, s := range all {
		if !s.IsValid() {
			t.Errorf("AllStatuses() lists %q but IsValid rejects it", s)
		}
	}

	// No duplicates: a duplicate would make AllStatuses lie about the count.
	seen := make(map[Status]bool, len(all))
	for _, s := range all {
		if seen[s] {
			t.Errorf("AllStatuses() lists %q more than once", s)
		}
		seen[s] = true
	}
}
