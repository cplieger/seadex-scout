package notify

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// ids builds a deletion-authority / incomplete set from AniList IDs.
func ids(alIDs ...int) map[int]struct{} {
	set := make(map[int]struct{}, len(alIDs))
	for _, id := range alIDs {
		set[id] = struct{}{}
	}
	return set
}

// TestReportScopedDeletesOnlyWithinItsAuthority is the reason ReportScoped
// exists.
//
// Report can delete a row by omission because a full pass looked at
// everything, so an absent row is a RESOLVED condition. A partial pass cannot
// make that inference: an entry it never examined is absent for exactly the
// same reason a resolved one is. Reading the two alike would stop reporting
// conditions that are still true (the alert clears while the problem stands)
// and then re-report them as new on the next full pass.
//
// comparedIDs is therefore deletion AUTHORITY, not a filter: a row whose owner
// is in the set and absent from the batch goes; a row whose owner is outside
// the set is carried untouched, and counted as carried.
func TestReportScopedDeletesOnlyWithinItsAuthority(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	inScope := findingWithID("in", "InScope", 1)
	outOfScope := findingWithID("out", "OutOfScope", 2)

	// A full pass first, so both rows are in the set.
	notifier.Report([]compare.Finding{inScope, outOfScope}, nil)

	// A window that examined ONLY entry 1, and found nothing for it. Entry 2
	// was never looked at.
	notifier.ReportScoped(nil, ids(1), nil)

	if got := titleCount(recorder, "InScope"); got != 1 {
		t.Errorf("in-authority row emitted %d times, want 1 (the full pass only; the scoped pass deleted it)", got)
	}
	if got := titleCount(recorder, "OutOfScope"); got != 2 {
		t.Errorf("out-of-authority row emitted %d times, want 2 (carried, because the window never examined its entry)", got)
	}
	if _, ok := notifier.current[dedupeKey(&inScope)]; ok {
		t.Error("the in-authority row survived the scoped pass, want it deleted by omission")
	}
	if _, ok := notifier.current[dedupeKey(&outOfScope)]; !ok {
		t.Error("the out-of-authority row was deleted, want it carried")
	}
	if carried, seen := summaryCounter(t, recorder, "carried"); !seen || carried != 1 {
		t.Errorf("summary carried = %d (present %v), want 1", carried, seen)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 1 {
		t.Errorf("summary total = %d, want 1 (only the carried row remains)", total)
	}
}

// TestReportScopedCarriesEveryOwnerOutsideAuthority is the table form of the
// case above across the four combinations of (owner in authority?) x (row in
// the new batch?), because the deletion decision is exactly that product and
// only one of the four cells deletes.
func TestReportScopedCarriesEveryOwnerOutsideAuthority(t *testing.T) {
	tests := map[string]struct {
		inAuthority bool
		reported    bool
		wantPresent bool
		wantCarried int64
	}{
		"examined and still reported":     {true, true, true, 0},
		"examined and no longer reported": {true, false, false, 0},
		"unexamined but reported anyway":  {false, true, true, 0},
		"unexamined and absent":           {false, false, true, 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			notifier, recorder := newCapturedNotifier()
			row := findingWithID("row", "Row", 7)
			notifier.Report([]compare.Finding{row}, nil)

			var authority map[int]struct{}
			if tc.inAuthority {
				authority = ids(7)
			} else {
				authority = ids(999)
			}
			var batch []compare.Finding
			if tc.reported {
				batch = []compare.Finding{row}
			}
			notifier.ReportScoped(batch, authority, nil)

			_, present := notifier.current[dedupeKey(&row)]
			if present != tc.wantPresent {
				t.Errorf("row present after the scoped pass = %v, want %v", present, tc.wantPresent)
			}
			if carried, _ := summaryCounter(t, recorder, "carried"); carried != tc.wantCarried {
				t.Errorf("summary carried = %d, want %d", carried, tc.wantCarried)
			}
		})
	}
}

// TestReportScopedPreservesIncompleteWithinAuthority pins the PRECEDENCE
// between the two preservation rules, which is the one place they can disagree.
//
// A row can be both in the window's authority AND owned by an entry whose
// evidence this pass found incomplete. Authority alone would delete it;
// incompleteness must win, exactly as it does in a full pass, because
// incomplete evidence means the pass cannot say the condition resolved. The
// counters keep the two apart - preserved for the incompleteness rule, carried
// for the authority rule - so an operator reading the summary can tell WHY a
// row survived.
func TestReportScopedPreservesIncompleteWithinAuthority(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	incomplete := findingWithID("inc", "Incomplete", 1)
	outOfScope := findingWithID("out", "OutOfScope", 2)

	notifier.Report([]compare.Finding{incomplete, outOfScope}, nil)

	// Entry 1 WAS examined (it is in the authority set) but its evidence was
	// incomplete, so its row must survive anyway.
	notifier.ReportScoped(nil, ids(1, 2), ids(1))

	if _, ok := notifier.current[dedupeKey(&incomplete)]; !ok {
		t.Error("an incomplete-evidence row inside the authority set was deleted; incompleteness must outrank authority")
	}
	if got := titleCount(recorder, "Incomplete"); got != 2 {
		t.Errorf("incomplete row emitted %d times, want 2 (preserved and re-emitted)", got)
	}
	// Entry 2 IS in the authority set this time and was not reported, so it is
	// deleted - which is what makes the survival above attributable to
	// incompleteness rather than to a blanket carry.
	if _, ok := notifier.current[dedupeKey(&outOfScope)]; ok {
		t.Error("an in-authority row with complete evidence survived, want it deleted by omission")
	}
	if preserved, seen := summaryCounter(t, recorder, "preserved"); !seen || preserved != 1 {
		t.Errorf("summary preserved = %d (present %v), want 1 (the incompleteness rule, not the authority rule)", preserved, seen)
	}
	if carried, _ := summaryCounter(t, recorder, "carried"); carried != 0 {
		t.Errorf("summary carried = %d, want 0 (every owner was inside the authority set)", carried)
	}
	// The in-authority row WAS resolved, and the count says which rule each
	// survivor fell under only if the third counter agrees.
	if resolved, seen := summaryCounter(t, recorder, "resolved"); !seen || resolved != 1 {
		t.Errorf("summary resolved = %d (present %v), want 1 (the in-authority row with complete evidence)", resolved, seen)
	}
}

// TestReportStillDeletesByOmission pins that adding the scoped path did not
// weaken the full one. Report passes nil authority, which the shared body reads
// as FULL deletion authority - so a row absent from a full pass still goes, and
// nothing is counted as carried.
//
// The distinction matters because nil and empty are opposites here: nil means
// "delete anything absent", empty means "delete nothing". A refactor that
// normalized nil to empty for tidiness would silently make every full pass stop
// resolving findings, and no count assertion on the full path alone would catch
// it.
func TestReportStillDeletesByOmission(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	kept := findingWithID("kept", "Kept", 1)
	gone := findingWithID("gone", "Gone", 2)

	notifier.Report([]compare.Finding{kept, gone}, nil)
	notifier.Report([]compare.Finding{kept}, nil)

	if _, ok := notifier.current[dedupeKey(&gone)]; ok {
		t.Error("a row absent from a full pass survived; Report must keep deleting by omission")
	}
	if got := titleCount(recorder, "Gone"); got != 1 {
		t.Errorf("the absent row was emitted %d times, want 1 (the first pass only)", got)
	}
	if carried, seen := summaryCounter(t, recorder, "carried"); !seen || carried != 0 {
		t.Errorf("summary carried = %d (present %v), want 0 on a full pass", carried, seen)
	}
	// The third counter of the same summary: a full pass that deleted a row must
	// SAY it resolved one, or the operator's "what changed this cycle" line
	// reports a silent deletion as nothing having happened.
	if resolved, seen := summaryCounter(t, recorder, "resolved"); !seen || resolved != 1 {
		t.Errorf("summary resolved = %d (present %v), want 1 (the omitted row)", resolved, seen)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 1 {
		t.Errorf("summary total = %d, want 1", total)
	}
}

// TestReportScopedNilAuthorityDeletesNothing pins the caller-bug arm. A scoped
// pass that computed no owner set has nothing to say about deletion, so a nil
// authority must NOT fall through to Report's full-deletion reading - it is
// normalized to the empty set, which carries every prior row.
//
// The safe direction is the point: getting this backwards would let a scoped
// caller whose owner-set computation returned nil wipe the whole finding set.
func TestReportScopedNilAuthorityDeletesNothing(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	row := findingWithID("row", "Row", 7)

	notifier.Report([]compare.Finding{row}, nil)
	notifier.ReportScoped(nil, nil, nil)

	if _, ok := notifier.current[dedupeKey(&row)]; !ok {
		t.Error("a nil authority set deleted a row; a scoped pass with no owner set may delete nothing")
	}
	if carried, _ := summaryCounter(t, recorder, "carried"); carried != 1 {
		t.Errorf("summary carried = %d, want 1", carried)
	}
}

// TestReportScopedAdmitsNewRowsRegardlessOfAuthority pins the direction
// authority does NOT govern. comparedIDs bounds DELETION only; a finding the
// window produced is always admitted, including for an owner outside the set
// (reachable when a window entry shares a dedupe key or an owner with one the
// window did not carry). Treating authority as an admission filter would make a
// tick unable to report the very change it just fetched.
func TestReportScopedAdmitsNewRowsRegardlessOfAuthority(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	fresh := findingWithID("fresh", "Fresh", 42)

	notifier.ReportScoped([]compare.Finding{fresh}, ids(1), nil)

	if _, ok := notifier.current[dedupeKey(&fresh)]; !ok {
		t.Error("a row the scoped pass produced was not admitted; authority bounds deletion, not admission")
	}
	if got := titleCount(recorder, "Fresh"); got != 1 {
		t.Errorf("the new row was emitted %d times, want 1", got)
	}
	if carried, _ := summaryCounter(t, recorder, "carried"); carried != 0 {
		t.Errorf("summary carried = %d, want 0 (nothing was carried; the row is new)", carried)
	}
}
