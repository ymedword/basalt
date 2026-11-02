// Package historyretention defines explicit, opt-in historical data maintenance.
// Current resources, credentials, knowledge and long-term memory are never candidates.
package historyretention

import (
	"context"
	"time"
)

var Groups = []string{"sessions", "runs", "ingest", "operations", "maintenance"}

// SessionGate shares admission exclusion with the only run entry point.
type SessionGate interface{ TrySessionMaintenance(string) (func(), bool) }

type Report struct {
	ID              string    `json:"id"`
	Group           string    `json:"group"`
	Cutoff          time.Time `json:"cutoff"`
	At              time.Time `json:"at"`
	DeletedGroups   int64     `json:"deleted_groups"`
	DeletedRows     int64     `json:"deleted_rows"`
	ProtectedGroups int64     `json:"protected_groups"`
	Status          string    `json:"status"`
	Reason          string    `json:"reason"`
	Next            string    `json:"-"`
	Scanned         int       `json:"-"`
}

type Store interface {
	CleanupHistoryBatch(context.Context, string, time.Time, int, string, SessionGate) (Report, error)
	HistoryReports(context.Context, time.Time, time.Time, int) ([]Report, error)
}
