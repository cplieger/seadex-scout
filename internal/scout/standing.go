package scout

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/state"
)

// msgStillStanding is the re-statement the alert rules count beside the
// escalated lines, so its text is part of the log contract.
const msgStillStanding = "degraded condition still standing"

// standingKey identifies one standing condition; arr is set only for the
// conditions that stand per arr.
type standingKey struct {
	cond degradation.Condition
	arr  string
}

// standingSet is every escalated condition, mapped to the pass that last
// observed it. It lets a pass whose observers did not run still state the
// condition, so every condition is logged once per pass whichever cadence
// observes it. It is persisted in state.State.Standing, so a restart does not
// end a condition no observer has seen end.
type standingSet map[standingKey]int

// observe records that pass escalated key itself.
func (s *standingSet) observe(key standingKey, pass int) {
	if *s == nil {
		*s = make(standingSet)
	}
	(*s)[key] = pass
}

// clear drops key once its observer has seen the condition end.
func (s standingSet) clear(key standingKey) {
	delete(s, key)
}

// has reports whether key is standing.
func (s standingSet) has(key standingKey) bool {
	_, ok := s[key]
	return ok
}

// restate emits msgStillStanding for every entry pass did not observe, in a
// stable order so a log reader sees the same sequence each pass.
func (s standingSet) restate(log *slog.Logger, pass int) {
	for _, key := range s.sorted() {
		if s[key] != pass {
			stateStanding(log, key)
		}
	}
}

// conditions returns the set in its persisted form, in restate's order.
func (s standingSet) conditions() []state.StandingCondition {
	keys := s.sorted()
	if len(keys) == 0 {
		return nil
	}
	out := make([]state.StandingCondition, len(keys))
	for i, key := range keys {
		out[i] = state.StandingCondition{Condition: key.cond, Arr: key.arr}
	}
	return out
}

func (s standingSet) sorted() []standingKey {
	keys := make([]standingKey, 0, len(s))
	for key := range s {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b standingKey) int {
		return cmp.Or(cmp.Compare(a.cond, b.cond), cmp.Compare(a.arr, b.arr))
	})
	return keys
}

func stateStanding(log *slog.Logger, key standingKey) {
	attrs := []any{degradation.AttrCondition, string(key.cond)}
	if key.arr != "" {
		attrs = append(attrs, "arr", key.arr)
	}
	log.Error(msgStillStanding, attrs...)
}

// restoreStanding seeds the set from the conditions an earlier process persisted
// and states each at once, so the alert's lookback spans the restart. It drops a
// condition this process has no observer to clear: the tick ones when no tick
// runs, and a shrink on an arr no longer enabled. A restored oversize streak
// resumes at the threshold so the next oversize tick re-escalates; unreachability
// needs no resume, as only a SeaDex read, which clears it, ends its standing.
func (s *Scout) restoreStanding(persisted []state.StandingCondition) {
	s.persistedStanding = persisted
	for _, c := range persisted {
		key := standingKey{cond: c.Condition, arr: c.Arr}
		if !s.observes(key) {
			continue
		}
		if key.cond == degradation.SeaDexWindowOversize {
			s.oversizeRun = s.latchTicks()
		}
		s.standing.observe(key, s.iterations)
		stateStanding(s.log, key)
	}
}

// observes reports whether this process runs the observer that can clear key.
func (s *Scout) observes(key standingKey) bool {
	switch key.cond {
	case degradation.SeaDexUnreachable, degradation.SeaDexWindowOversize:
		return key.arr == "" && s.pollInterval > 0
	case degradation.LibraryWalkShrunk:
		return slices.Contains(s.library.EnabledArrs(), key.arr)
	case degradation.SeaDexCatalogueFetchFailing, degradation.MappingRefreshRejected,
		degradation.AniListLookupsFailing, degradation.LibraryWalkPartial:
		return key.arr == ""
	default:
		return false
	}
}

// persistStanding writes the set when it differs from what state.json holds. It
// is needed only on a pass that changed the set without saving: a tick that
// escalated or cleared a tick condition before its state load.
func (s *Scout) persistStanding(ctx context.Context) {
	if slices.Equal(s.standing.conditions(), s.persistedStanding) {
		return
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		// Saving a fallback empty state would discard the memo and the snapshot.
		s.log.Warn("standing conditions not persisted; state load failed", "error", err)
		return
	}
	s.save(ctx, &st)
}
