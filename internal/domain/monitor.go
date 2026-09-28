package domain

import (
	"slices"
	"strconv"
)

// MonitorKind is which family of monitor something is about.
//
// The two families are one concept with two implementations. Both answer "is
// this thing still working"; they differ only in who speaks first — a cron
// monitor waits to be told, an uptime monitor goes and asks. Everything
// downstream treats them alike: the same scopes, the same silence window, the
// same alert payload, the same panel screen, the same status page. So the
// discriminator lives here, once, instead of each family growing its own
// parallel copy of the machinery around it.
//
// It is not decoration. The two families number their monitors from separate
// tables, so cron monitor 7 and uptime monitor 7 both exist and are unrelated.
// Without the kind, one of them going down would silence the other's alert
// —the silence window is keyed on the subject— and a notification's link would
// open the wrong screen (ADR 015, ADR 016).
type MonitorKind string

const (
	// MonitorKindCron is a monitor that watches for something that reports in.
	MonitorKindCron MonitorKind = "cron"
	// MonitorKindUptime is a monitor that watches by asking.
	MonitorKindUptime MonitorKind = "uptime"
)

// AllMonitorKinds lists every family this build has.
func AllMonitorKinds() []MonitorKind {
	return []MonitorKind{MonitorKindCron, MonitorKindUptime}
}

// Valid reports whether k is a family this build knows.
func (k MonitorKind) Valid() bool { return slices.Contains(AllMonitorKinds(), k) }

// MonitorSubjectKey identifies a monitor to the silence window (ADR 015).
//
// The kind is in the key, not just the id, for the reason MonitorKind gives:
// the ids come from two tables and collide.
func MonitorSubjectKey(kind MonitorKind, monitorID int64) string {
	return "monitor:" + string(kind) + ":" + strconv.FormatInt(monitorID, 10)
}
