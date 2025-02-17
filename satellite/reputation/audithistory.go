// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package reputation

import (
	"time"

	"storj.io/common/pb"
)

// AuditHistory represents a node's audit history for the most recent tracking period.
type AuditHistory struct {
	Score   float64
	Windows []*AuditWindow
}

// UpdateAuditHistoryResponse contains information returned by UpdateAuditHistory.
type UpdateAuditHistoryResponse struct {
	NewScore           float64
	TrackingPeriodFull bool
	History            []byte
}

// AuditWindow represents the number of online and total audits a node received for a specific time period.
type AuditWindow struct {
	WindowStart time.Time
	TotalCount  int32
	OnlineCount int32
}

// AuditHistoryToPB converts an overlay.AuditHistory to a pb.AuditHistory.
func AuditHistoryToPB(auditHistory AuditHistory) (historyPB *pb.AuditHistory) {
	historyPB = &pb.AuditHistory{
		Score:   auditHistory.Score,
		Windows: make([]*pb.AuditWindow, len(auditHistory.Windows)),
	}
	for i, window := range auditHistory.Windows {
		historyPB.Windows[i] = &pb.AuditWindow{
			TotalCount:  window.TotalCount,
			OnlineCount: window.OnlineCount,
			WindowStart: window.WindowStart,
		}
	}
	return historyPB
}
