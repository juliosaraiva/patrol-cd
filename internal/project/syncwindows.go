package project

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones must resolve in minimal images, or windows silently fall back to UTC.

	"github.com/robfig/cron/v3"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Sync window kinds.
const (
	SyncWindowKindAllow = "allow"
	SyncWindowKindDeny  = "deny"
)

// Messages used at the sync window enforcement points (TEN-023, SYNC-087).
const (
	// SyncBlockedAPIMessage is the PermissionDenied message of a manual sync request refused
	// by the API server.
	SyncBlockedAPIMessage = "cannot sync: blocked by sync window"
	// SyncBlockedOperationMessage is the message of a Running operation paused by a window;
	// the controller appends ": <err>" when the windows could not be evaluated.
	SyncBlockedOperationMessage = "Sync operation blocked by sync window"
)

// cronParser has the upstream parser options: minute, hour, day of month, month and day of
// week; no seconds field and no descriptors (@daily, @every ...). A "TZ=<zone> " or
// "CRON_TZ=<zone> " prefix is honored by robfig/cron itself.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type cachedSchedule struct {
	sched cron.Schedule
	err   error
}

// scheduleCache holds parsed schedules (immutable once parsed, safe to share).
var scheduleCache = newCache[string, cachedSchedule](1024)

// parseSchedule parses a window schedule with the upstream parser, through a bounded cache.
func parseSchedule(spec string) (cron.Schedule, error) {
	if c, ok := scheduleCache.Get(spec); ok {
		return c.sched, c.err
	}
	sched, err := parseScheduleUncached(spec)
	scheduleCache.Add(spec, cachedSchedule{sched: sched, err: err})
	return sched, err
}

// parseScheduleUncached parses with robfig/cron. It panics on a "TZ=..." prefix without a
// following space; that input is rejected here instead, and any other parser panic is
// converted to an error because schedules are untrusted input.
func parseScheduleUncached(spec string) (sched cron.Schedule, err error) {
	if (strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")) && !strings.Contains(spec, " ") {
		return nil, fmt.Errorf("provided bad location %s: missing schedule after the time zone", spec[strings.Index(spec, "=")+1:])
	}
	defer func() {
		if r := recover(); r != nil {
			sched, err = nil, fmt.Errorf("invalid schedule: %v", r)
		}
	}()
	return cronParser.Parse(spec)
}

// parseWindow parses schedule and duration with the upstream error messages.
func parseWindow(w *v1alpha1.SyncWindow) (cron.Schedule, time.Duration, error) {
	sched, err := parseSchedule(w.Schedule)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot parse schedule '%s': %w", w.Schedule, err)
	}
	d, err := time.ParseDuration(w.Duration)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot parse duration '%s': %w", w.Duration, err)
	}
	return sched, d, nil
}

type cachedLocation struct {
	loc *time.Location
	err error
}

var locationCache = newCache[string, cachedLocation](256)

// loadLocation is time.LoadLocation with a bounded cache. Upstream falls back to UTC with a
// warning when the zone is unknown; the warning is logged once per zone name here.
func loadLocation(name string) (*time.Location, bool) {
	c, ok := locationCache.Get(name)
	if !ok {
		loc, err := time.LoadLocation(name)
		c = cachedLocation{loc: loc, err: err}
		locationCache.Add(name, c)
		if err != nil {
			slog.Default().Warn("project: invalid sync window time zone, using UTC", "timeZone", name, "error", err)
		}
	}
	if c.err != nil {
		return time.UTC, false
	}
	return c.loc, true
}

// timeZoneOffset is upstream scheduleOffsetByTimeZone: the UTC offset of the window's time
// zone. Upstream takes it at wall-clock "now"; here it is taken at the evaluation instant
// passed by the caller (which is time.Now() in production), so a DST change shifts every
// evaluation made after it, including the "allowed at operation start" overrun check.
func timeZoneOffset(tz string, at time.Time) time.Duration {
	loc, _ := loadLocation(tz)
	_, off := at.In(loc).Zone()
	return time.Duration(off) * time.Second
}

// windowActiveAt reports whether w is active at checkTime using the time-zone offset taken at
// offsetAt. Upstream: Next(now+offset-duration) is before now+offset, so a window opened by a
// fire at f is active for instants strictly between f and f+duration (shifted by the offset).
func windowActiveAt(w *v1alpha1.SyncWindow, checkTime, offsetAt time.Time) (bool, error) {
	sched, d, err := parseWindow(w)
	if err != nil {
		return false, err
	}
	shifted := checkTime.UTC().Add(timeZoneOffset(w.TimeZone, offsetAt))
	// robfig/cron returns the zero time for a schedule that never fires (e.g. "0 0 30 2 *");
	// the zero time is before any instant, so upstream treats such a window as always
	// active. Kept for parity.
	return sched.Next(shifted.Add(-d)).Before(shifted), nil
}

// SyncWindowActive reports whether w is active at now (upstream SyncWindow.Active).
func SyncWindowActive(w v1alpha1.SyncWindow, now time.Time) (bool, error) {
	return windowActiveAt(&w, now, now)
}

func activeAt(windows v1alpha1.SyncWindows, checkTime, offsetAt time.Time) (v1alpha1.SyncWindows, error) {
	var active v1alpha1.SyncWindows
	for i := range windows {
		ok, err := windowActiveAt(&windows[i], checkTime, offsetAt)
		if err != nil {
			return nil, err
		}
		if ok {
			active = append(active, windows[i])
		}
	}
	return active, nil
}

func inactiveAllowsAt(windows v1alpha1.SyncWindows, checkTime, offsetAt time.Time) (v1alpha1.SyncWindows, error) {
	var inactive v1alpha1.SyncWindows
	for i := range windows {
		if windows[i].Kind != SyncWindowKindAllow {
			continue
		}
		ok, err := windowActiveAt(&windows[i], checkTime, offsetAt)
		if err != nil {
			return nil, err
		}
		if !ok {
			inactive = append(inactive, windows[i])
		}
	}
	return inactive, nil
}

// ActiveSyncWindows returns the windows active at now, in order (upstream
// SyncWindows.Active). Any unparsable window (of any kind) is an error.
func ActiveSyncWindows(windows v1alpha1.SyncWindows, now time.Time) (v1alpha1.SyncWindows, error) {
	return activeAt(windows, now, now)
}

// InactiveAllowSyncWindows returns the allow windows that are not active at now (upstream
// SyncWindows.InactiveAllows).
func InactiveAllowSyncWindows(windows v1alpha1.SyncWindows, now time.Time) (v1alpha1.SyncWindows, error) {
	return inactiveAllowsAt(windows, now, now)
}

// MatchSyncWindows returns the windows that apply to app (upstream SyncWindows.Matches).
// Selectors are globs without negation: applications against metadata.name, clusters against
// spec.destination.name or spec.destination.server as written (not the resolved cluster),
// namespaces against spec.destination.namespace. By default a window applies when any
// configured selector matches; with andOperator every configured (non-empty) selector must
// match. A window without selectors never applies.
func MatchSyncWindows(windows v1alpha1.SyncWindows, app *v1alpha1.Application) v1alpha1.SyncWindows {
	if app == nil {
		return nil
	}
	var out v1alpha1.SyncWindows
	dst := app.Spec.Destination
	for _, w := range windows {
		matched, isSet := false, false

		if len(w.Applications) > 0 {
			isSet = true
			for _, a := range w.Applications {
				if matchPattern(a, app.Name, false) {
					matched = true
					break
				}
			}
		}
		if w.UseAndOperator && !matched && isSet {
			continue
		} else if !w.UseAndOperator && matched {
			out = append(out, w)
			continue
		}

		if len(w.Clusters) > 0 {
			matched, isSet = false, true
			for _, c := range w.Clusters {
				if (dst.Name != "" && matchPattern(c, dst.Name, false)) || (dst.Server != "" && matchPattern(c, dst.Server, false)) {
					matched = true
					break
				}
			}
		}
		if isSet && w.UseAndOperator && !matched {
			continue
		} else if !w.UseAndOperator && matched {
			out = append(out, w)
			continue
		}

		if len(w.Namespaces) > 0 {
			matched = false
			for _, n := range w.Namespaces {
				if matchPattern(n, dst.Namespace, false) {
					matched = true
					break
				}
			}
		}
		if matched {
			out = append(out, w)
		}
	}
	return out
}

// hasDeny reports whether a deny window is present and whether every deny window allows
// manual syncs.
func hasDeny(windows v1alpha1.SyncWindows) (found, manualEnabled bool) {
	for _, w := range windows {
		if w.Kind != SyncWindowKindDeny {
			continue
		}
		if !found {
			manualEnabled = w.ManualSync
		} else if manualEnabled {
			manualEnabled = w.ManualSync
		}
		found = true
	}
	return found, manualEnabled
}

func hasAllow(windows v1alpha1.SyncWindows) bool {
	for _, w := range windows {
		if w.Kind == SyncWindowKindAllow {
			return true
		}
	}
	return false
}

// allManual reports whether every window has manualSync (false for an empty list).
func allManual(windows v1alpha1.SyncWindows) bool {
	if len(windows) == 0 {
		return false
	}
	for _, w := range windows {
		if !w.ManualSync {
			return false
		}
	}
	return true
}

// allKindOverrun reports whether windows contain at least one window of kind and every
// window of that kind has syncOverrun.
func allKindOverrun(windows v1alpha1.SyncWindows, kind string) bool {
	found := false
	for _, w := range windows {
		if w.Kind != kind {
			continue
		}
		found = true
		if !w.SyncOverrun {
			return false
		}
	}
	return found
}

// CanSyncWithWindows reports whether windows (normally the windows matching one Application,
// see MatchSyncWindows) allow a sync at now (upstream SyncWindows.CanSync):
//
//   - no windows: allowed;
//   - an active deny window: denied, unless the sync is manual and every active deny window
//     has manualSync, or (overrun) operationStart is set, every active deny window has
//     syncOverrun and the sync would have been allowed at operationStart;
//   - otherwise an active allow window: allowed;
//   - otherwise, when allow windows exist (all inactive): denied, unless manual and every
//     inactive allow window has manualSync, or (overrun) every inactive allow window has
//     syncOverrun and the sync was allowed at operationStart;
//   - otherwise allowed.
//
// operationStart is only set for an operation already running (controller); new sync
// requests pass nil and never benefit from overrun. Windows that cannot be parsed make the
// result false with an error starting "invalid sync windows: ".
func CanSyncWithWindows(windows v1alpha1.SyncWindows, manual bool, operationStart *time.Time, now time.Time) (bool, error) {
	if len(windows) == 0 {
		return true, nil
	}
	active, err := activeAt(windows, now, now)
	if err != nil {
		return false, fmt.Errorf("invalid sync windows: %w", err)
	}
	if deny, manualEnabled := hasDeny(active); deny {
		if manual && manualEnabled {
			return true, nil
		}
		if operationStart != nil && !operationStart.IsZero() && allKindOverrun(active, SyncWindowKindDeny) {
			wasAllowed, err := canSyncAt(windows, manual, *operationStart, now)
			if err != nil {
				return false, err
			}
			if wasAllowed {
				return true, nil
			}
		}
		return false, nil
	}
	if hasAllow(active) {
		return true, nil
	}
	inactive, err := inactiveAllowsAt(windows, now, now)
	if err != nil {
		return false, fmt.Errorf("invalid sync windows: %w", err)
	}
	if len(inactive) > 0 {
		if manual && allManual(inactive) {
			return true, nil
		}
		if operationStart != nil && !operationStart.IsZero() && allKindOverrun(inactive, SyncWindowKindAllow) {
			wasAllowed, err := canSyncAt(windows, manual, *operationStart, now)
			if err != nil {
				return false, err
			}
			if wasAllowed {
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}

// canSyncAt is upstream canSyncAtTime: the decision at checkTime without overrun, with the
// time-zone offsets taken at now.
func canSyncAt(windows v1alpha1.SyncWindows, manual bool, checkTime, now time.Time) (bool, error) {
	active, err := activeAt(windows, checkTime, now)
	if err != nil {
		return false, fmt.Errorf("invalid sync windows: %w", err)
	}
	if deny, manualEnabled := hasDeny(active); deny {
		return manual && manualEnabled, nil
	}
	if hasAllow(active) {
		return true, nil
	}
	inactive, err := inactiveAllowsAt(windows, checkTime, now)
	if err != nil {
		return false, fmt.Errorf("invalid sync windows: %w", err)
	}
	if len(inactive) > 0 {
		return manual && allManual(inactive), nil
	}
	return true, nil
}

// ActiveWindows returns the windows of proj that apply to app and are active at now.
// proj should be the effective project (global projects merged, see VirtualProject).
func ActiveWindows(proj *v1alpha1.AppProject, app *v1alpha1.Application, now time.Time) (v1alpha1.SyncWindows, error) {
	if proj == nil {
		return nil, nil
	}
	return ActiveSyncWindows(MatchSyncWindows(proj.Spec.SyncWindows, app), now)
}

// CanSync reports whether the sync windows of proj allow syncing app at now. manual is true
// for user-initiated syncs; operationStart is the start time of a running operation (nil for
// a new sync request or an automated sync decision), which enables syncOverrun.
func CanSync(proj *v1alpha1.AppProject, app *v1alpha1.Application, manual bool, operationStart *time.Time, now time.Time) (bool, error) {
	if proj == nil {
		return true, nil
	}
	return CanSyncWithWindows(MatchSyncWindows(proj.Spec.SyncWindows, app), manual, operationStart, now)
}

// SyncWindowStatus summarizes the sync window state of an Application for the CLI and UI.
type SyncWindowStatus string

// The CLI wording (argocd app get "SyncWindow:"). The UI colors map Allowed → green,
// ManualAllowed → orange, Denied → red.
const (
	SyncWindowStatusAllowed       SyncWindowStatus = "Sync Allowed"
	SyncWindowStatusManualAllowed SyncWindowStatus = "Manual Allowed"
	SyncWindowStatusDenied        SyncWindowStatus = "Sync Denied"
)

// ApplicationSyncWindowsState is the answer of GET /applications/{name}/syncwindows plus the
// summary shown by the CLI/UI and the next instant the state may change.
type ApplicationSyncWindowsState struct {
	// AssignedWindows are the project windows matching the Application.
	AssignedWindows v1alpha1.SyncWindows
	// ActiveWindows are the assigned windows active now.
	ActiveWindows v1alpha1.SyncWindows
	// CanSync is the manual-sync decision (no operation start time), as upstream.
	CanSync bool
	// Status is the CLI/UI summary.
	Status SyncWindowStatus
	// NextTransition is the earliest instant after which an assigned window opens or closes
	// (nil when none within the search horizon). Patrol addition: the controller requeues
	// Pending operations at that time and the UI shows it.
	NextTransition *time.Time
}

// GetApplicationSyncWindowsState evaluates the windows of proj for app at now. Errors carry
// the upstream API wording ("invalid sync windows: ...").
func GetApplicationSyncWindowsState(proj *v1alpha1.AppProject, app *v1alpha1.Application, now time.Time) (*ApplicationSyncWindowsState, error) {
	var assigned v1alpha1.SyncWindows
	if proj != nil {
		assigned = MatchSyncWindows(proj.Spec.SyncWindows, app)
	}
	canSync, err := CanSyncWithWindows(assigned, true, nil, now)
	if err != nil {
		// Upstream wraps the already wrapped CanSync error again.
		return nil, fmt.Errorf("invalid sync windows: %w", err)
	}
	active, err := ActiveSyncWindows(assigned, now)
	if err != nil {
		return nil, fmt.Errorf("invalid sync windows: %w", err)
	}
	state := &ApplicationSyncWindowsState{AssignedWindows: assigned, ActiveWindows: active, CanSync: canSync, Status: SyncWindowStatusAllowed}
	if len(assigned) > 0 {
		var deny, allow bool
		for _, w := range active {
			if w.Kind == SyncWindowKindDeny {
				deny = true
			} else {
				allow = true
			}
		}
		inactive, err := InactiveAllowSyncWindows(assigned, now)
		if err != nil {
			return nil, fmt.Errorf("invalid sync windows: %w", err)
		}
		if deny || (!allow && len(inactive) > 0) {
			state.Status = SyncWindowStatusDenied
			if canSync {
				state.Status = SyncWindowStatusManualAllowed
			}
		}
	}
	if t, ok, err := NextSyncWindowTransition(assigned, now); err == nil && ok {
		state.NextTransition = &t
	}
	return state, nil
}

// maxMergedOccurrences bounds the walk over overlapping occurrences when computing when a
// window closes (an every-minute schedule with a long duration never closes): six hours of
// every-minute fires, or two weeks of hourly ones. The worst case costs about 3ms; callers
// treat a zero End as "re-evaluate later".
const maxMergedOccurrences = 360

// SyncWindowTimes describes the current or next occurrence of a window.
type SyncWindowTimes struct {
	// Active is SyncWindowActive at the evaluation time.
	Active bool
	// Start is the opening of the earliest occurrence still covering the evaluation time
	// when Active, else of the next occurrence. The window is active strictly after Start.
	// Zero when the duration is not positive (never active) or the schedule never fires
	// (always active, as upstream treats it).
	Start time.Time
	// End is when the window closes, following overlapping occurrences; the window is
	// inactive at End. Zero when it does not close within the search horizon.
	End time.Time
}

// GetSyncWindowTimes computes the current or next occurrence of w at now, with the same
// time-zone semantics as SyncWindowActive (offset taken at now). Times are in UTC.
func GetSyncWindowTimes(w v1alpha1.SyncWindow, now time.Time) (SyncWindowTimes, error) {
	sched, d, err := parseWindow(&w)
	if err != nil {
		return SyncWindowTimes{}, err
	}
	offset := timeZoneOffset(w.TimeZone, now)
	shifted := now.UTC().Add(offset)
	first := sched.Next(shifted.Add(-d))
	if first.IsZero() {
		// Never fires: permanently active, as SyncWindowActive reports (see windowActiveAt).
		return SyncWindowTimes{Active: true}, nil
	}
	if d <= 0 {
		// Next(t-d) is after t: such a window is never active.
		return SyncWindowTimes{}, nil
	}
	times := SyncWindowTimes{Active: first.Before(shifted), Start: first.Add(-offset).UTC()}
	end, fire := first.Add(d), first
	for range maxMergedOccurrences {
		next := sched.Next(fire)
		if next.IsZero() || !next.Before(end) {
			times.End = end.Add(-offset).UTC()
			return times, nil
		}
		fire, end = next, next.Add(d)
	}
	return times, nil
}

// NextSyncWindowTransition returns the earliest instant, at or after now, at which one of
// windows opens or closes. ok is false when no window changes state within the search
// horizon.
func NextSyncWindowTransition(windows v1alpha1.SyncWindows, now time.Time) (time.Time, bool, error) {
	var best time.Time
	found := false
	for _, w := range windows {
		t, err := GetSyncWindowTimes(w, now)
		if err != nil {
			return time.Time{}, false, err
		}
		candidate := t.Start
		if t.Active {
			candidate = t.End
		}
		if candidate.IsZero() {
			continue
		}
		if !found || candidate.Before(best) {
			best, found = candidate, true
		}
	}
	return best, found, nil
}
