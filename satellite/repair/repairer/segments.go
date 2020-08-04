// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package repairer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"storj.io/common/uuid"
	"storj.io/storj/satellite/internalpb"

	"storj.io/common/pb"
	"storj.io/common/storj"
	"storj.io/storj/satellite/audit"
	"storj.io/storj/satellite/metabase"
	"storj.io/storj/satellite/orders"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/repair/checker"
	"storj.io/storj/satellite/repair/queue"
	"storj.io/uplink/private/eestream"
)

var (
	metainfoGetError       = errs.Class("metainfo db get")
	metainfoPutError       = errs.Class("metainfo db put")
	invalidRepairError     = errs.Class("invalid repair")
	overlayQueryError      = errs.Class("overlay query failure")
	orderLimitFailureError = errs.Class("order limits failure")
	repairReconstructError = errs.Class("repair reconstruction failure")
	repairPutError         = errs.Class("repair could not store repaired pieces")
	// segmentVerificationError is the errs class when the repaired segment can not be verified during repair.
	segmentVerificationError = errs.Class("segment verification failed")
	// segmentDeletedError is the errs class when the repaired segment was deleted during the repair.
	segmentDeletedError = errs.Class("segment deleted during repair")
	// segmentModifiedError is the errs class used when a segment has been changed in any way.
	segmentModifiedError = errs.Class("segment has been modified")
	repairResultError    = errs.Class("invalid repair result")
)

// irreparableError identifies situations where a segment could not be repaired due to reasons
// which are hopefully transient (e.g. too many pieces unavailable). The segment should be added
// to the irreparableDB.
type irreparableError struct {
	piecesAvailable int32
	piecesRequired  int32
	errlist         []error
}

func (ie *irreparableError) Error() string {
	return fmt.Sprintf("%d available pieces < %d required", ie.piecesAvailable, ie.piecesRequired)
}

// SegmentRepairer for segments.
type SegmentRepairer struct {
	log            *zap.Logger
	statsCollector *statsCollector
	metabase       *metabase.DB
	orders         *orders.Service
	overlay        *overlay.Service
	ec             *ECRepairer
	timeout        time.Duration
	reporter       *audit.Reporter

	// multiplierOptimalThreshold is the value that multiplied by the optimal
	// threshold results in the maximum limit of number of nodes to upload
	// repaired pieces
	multiplierOptimalThreshold float64

	// repairOverrides is the set of values configured by the checker to override the repair threshold for various RS schemes.
	repairOverrides checker.RepairOverridesMap

	nowFn                            func() time.Time
	OnTestingCheckSegmentAlteredHook func()
	OnTestingPiecesReportHook        func(pieces audit.Pieces)
}

// NewSegmentRepairer creates a new instance of SegmentRepairer.
//
// excessOptimalThreshold is the factor to apply over the optimal threshold
// to determine the maximum limit of nodes to upload repaired pieces. For
// example, excessOptimalThreshold = 0.25 will add 25% to the optimal threshold
// and use the product as the target piece count. When negative, 0 is applied.
func NewSegmentRepairer(
	log *zap.Logger,
	metabase *metabase.DB,
	orders *orders.Service,
	overlay *overlay.Service,
	reporter *audit.Reporter,
	ecRepairer *ECRepairer,
	repairOverrides checker.RepairOverrides,
	timeout time.Duration, excessOptimalThreshold float64,
) *SegmentRepairer {

	if excessOptimalThreshold < 0 {
		excessOptimalThreshold = 0
	}

	return &SegmentRepairer{
		log:                        log,
		statsCollector:             newStatsCollector(),
		metabase:                   metabase,
		orders:                     orders,
		overlay:                    overlay,
		ec:                         ecRepairer,
		timeout:                    timeout,
		multiplierOptimalThreshold: 1 + excessOptimalThreshold,
		repairOverrides:            repairOverrides.GetMap(),
		reporter:                   reporter,

		nowFn: time.Now,
	}
}

// Repair retrieves an at-risk segment and repairs and stores lost pieces on new nodes
// note that shouldDelete is used even in the case where err is not null
// note that it will update audit status as failed for nodes that failed piece hash verification during repair downloading.
func (repairer *SegmentRepairer) Repair(ctx context.Context, queueSegment *queue.InjuredSegment) (shouldDelete bool, repairPerformed bool, err error) {
	defer mon.Task()(&ctx, queueSegment.StreamID.String(), queueSegment.Position.Encode())(&err)

	shouldDelete, jobDefinition, pointer, healthyPieceNums, err := repairer.PrepareRepairJob(ctx, path)
	if err != nil || shouldDelete {
		return shouldDelete, false, err
	}

	repairResult := PerformRepairJob(ctx, repairer.ec, jobDefinition)

	shouldDelete, err = repairer.ProcessRepairResult(ctx, path, pointer, healthyPieceNums, repairResult)
	return shouldDelete, true, err
}

// PrepareRepairJob gathers the necessary information for a repair job before
// repair actually starts. This includes everything necessary to fill in a
// internalpb.RepairJobDefinition instance.
//
// If shouldDelete is returned as true, the caller can delete the path from
// the repair queue and move on to the next. It is either currently irreparable
// (if err is an irreparableError) or the repair no longer needs to be
// accomplished.
func (repairer *SegmentRepairer) PrepareRepairJob(ctx context.Context, path storj.Path) (shouldDelete bool, jobDefinition *internalpb.RepairJobDefinition, pointer *pb.Pointer, healthyPieceNums []int32, err error) {
	defer mon.Task()(&ctx)(&err)

	segment, err := repairer.metabase.GetSegmentByPosition(ctx, metabase.GetSegmentByPosition{
		StreamID: queueSegment.StreamID,
		Position: queueSegment.Position,
	})
	if err != nil {
		if metabase.ErrSegmentNotFound.Has(err) {
			mon.Meter("repair_unnecessary").Mark(1)            //mon:locked
			mon.Meter("segment_deleted_before_repair").Mark(1) //mon:locked
			repairer.log.Debug("segment was deleted")
			return true, nil, nil, nil, nil
		}
		return false, nil, nil, nil, metainfoGetError.Wrap(err)
	}

	if segment.Inline() {
		return true, nil, nil, nil, invalidRepairError.New("cannot repair inline segment")
	}

	// ignore segment if expired
	if segment.Expired(repairer.nowFn()) {
		repairer.log.Debug("segment has expired", zap.Stringer("Stream ID", segment.StreamID), zap.Uint64("Position", queueSegment.Position.Encode()))
		return true, nil
	}

	redundancy, err := eestream.NewRedundancyStrategyFromStorj(segment.Redundancy)
	if err != nil {
		return true, nil, nil, nil, invalidRepairError.New("invalid redundancy strategy: %w", err)
	}

	stats := repairer.getStatsByRS(&pb.RedundancyScheme{
		Type:             pb.RedundancyScheme_SchemeType(segment.Redundancy.Algorithm),
		ErasureShareSize: segment.Redundancy.ShareSize,
		MinReq:           int32(segment.Redundancy.RequiredShares),
		RepairThreshold:  int32(segment.Redundancy.RepairShares),
		SuccessThreshold: int32(segment.Redundancy.OptimalShares),
		Total:            int32(segment.Redundancy.TotalShares),
	})

	mon.Meter("repair_attempts").Mark(1) //mon:locked
	stats.repairAttempts.Mark(1)
	mon.IntVal("repair_segment_size").Observe(int64(segment.EncryptedSize)) //mon:locked
	stats.repairSegmentSize.Observe(int64(segment.EncryptedSize))

	var excludeNodeIDs storj.NodeIDList
	var healthyPieces []*pb.RemotePiece

	pieces := segment.Pieces
	missingPieces, err := repairer.overlay.GetMissingPieces(ctx, pieces)
	if err != nil {
		return false, nil, nil, nil, overlayQueryError.New("error identifying missing pieces: %w", err)
	}

	numHealthy := len(pieces) - len(missingPieces)
	// irreparable piece
	if numHealthy < int(segment.Redundancy.RequiredShares) {
		mon.Counter("repairer_segments_below_min_req").Inc(1) //mon:locked
		mon.Meter("repair_nodes_unavailable").Mark(1)         //mon:locked
		mon.Meter("builtin_repair_nodes_unavailable").Mark(1) //mon:locked
		stats.repairerSegmentsBelowMinReq.Inc(1)
		stats.repairerNodesUnavailable.Mark(1)

		repairer.log.Warn("irreparable segment",
			zap.String("StreamID", queueSegment.StreamID.String()),
			zap.Uint64("Position", queueSegment.Position.Encode()),
			zap.Int("piecesAvailable", numHealthy),
			zap.Int16("piecesRequired", segment.Redundancy.RequiredShares),
		)
		return false, nil, nil, nil, nil
	}

	// ensure we get values, even if only zero values, so that redash can have an alert based on this
	mon.Counter("repairer_segments_below_min_req").Inc(0) //mon:locked
	stats.repairerSegmentsBelowMinReq.Inc(0)

	repairThreshold := int32(segment.Redundancy.RepairShares)

	pbRedundancy := &pb.RedundancyScheme{
		MinReq:           int32(segment.Redundancy.RequiredShares),
		RepairThreshold:  int32(segment.Redundancy.RepairShares),
		SuccessThreshold: int32(segment.Redundancy.OptimalShares),
		Total:            int32(segment.Redundancy.TotalShares),
	}
	overrideValue := repairer.repairOverrides.GetOverrideValuePB(pbRedundancy)
	if overrideValue != 0 {
		repairThreshold = overrideValue
	}

	// repair not needed
	if numHealthy > int(repairThreshold) {
		mon.Meter("repair_unnecessary").Mark(1)         //mon:locked
		mon.Meter("builtin_repair_unnecessary").Mark(1) //mon:locked
		stats.repairUnnecessary.Mark(1)
		repairer.log.Debug("segment above repair threshold", zap.Int("numHealthy", numHealthy), zap.Int32("repairThreshold", repairThreshold))
		return true, nil, nil, nil, nil
	}

	healthyRatioBeforeRepair := 0.0
	if segment.Redundancy.TotalShares != 0 {
		healthyRatioBeforeRepair = float64(numHealthy) / float64(segment.Redundancy.TotalShares)
	}
	mon.FloatVal("healthy_ratio_before_repair").Observe(healthyRatioBeforeRepair)         //mon:locked
	mon.FloatVal("builtin_healthy_ratio_before_repair").Observe(healthyRatioBeforeRepair) //mon:locked
	stats.healthyRatioBeforeRepair.Observe(healthyRatioBeforeRepair)

	lostPiecesSet := sliceToSet(missingPieces)

	var healthyPieces, unhealthyPieces metabase.Pieces
	// Populate healthyPieces with all pieces from the segment except those correlating to indices in lostPieces
	for _, piece := range pieces {
		excludeNodeIDs = append(excludeNodeIDs, piece.StorageNode)
		if !lostPiecesSet[piece.Number] {
			healthyPieces = append(healthyPieces, piece)
		}
	}

	// Create the order limits for the GET_REPAIR action
	getOrderLimits, getPrivateKey, cachedIPsAndPorts, err := repairer.orders.CreateGetRepairOrderLimits(ctx, metabase.BucketLocation{}, segment, healthyPieces)
	if err != nil {
		if orders.ErrDownloadFailedNotEnoughPieces.Has(err) {
			mon.Counter("repairer_segments_below_min_req").Inc(1) //mon:locked
			stats.repairerSegmentsBelowMinReq.Inc(1)
			mon.Meter("repair_nodes_unavailable").Mark(1) //mon:locked
			stats.repairerNodesUnavailable.Mark(1)

			repairer.log.Warn("irreparable segment",
				zap.String("StreamID", queueSegment.StreamID.String()),
				zap.Uint64("Position", queueSegment.Position.Encode()),
				zap.Error(err),
			)
		}
		return false, nil, nil, nil, orderLimitFailureError.New("could not create GET_REPAIR order limits: %w", err)
	}

	// Some pieces may have been recognized as unhealthy inside CreateGetRepairOrderLimits.
	// Refresh our idea of what pieces are healthy with that information.
	healthyPieces = nil // no longer helpful or valid; try to be sure it's not used later
	for i, piece := range getOrderLimits {
		if piece != nil {
			healthyPieceNums = append(healthyPieceNums, int32(i))
		}
	}

	var requestCount int
	var minSuccessfulNeeded int
	{
		totalNeeded := math.Ceil(float64(redundancy.OptimalThreshold()) * repairer.multiplierOptimalThreshold)
		requestCount = int(totalNeeded) - len(healthyPieceNums)
		minSuccessfulNeeded = redundancy.OptimalThreshold() - len(healthyPieceNums)
	}

	// Request Overlay for n-h new storage nodes
	request := overlay.FindStorageNodesRequest{
		RequestedCount: requestCount,
		ExcludedIDs:    excludeNodeIDs,
	}
	newNodes, err := repairer.overlay.FindStorageNodesForUpload(ctx, request)
	if err != nil {
		return false, nil, nil, nil, overlayQueryError.Wrap(err)
	}

	// Create the order limits for the PUT_REPAIR action
	putLimits, putPrivateKey, err := repairer.orders.CreatePutRepairOrderLimits(ctx, metabase.BucketLocation{}, segment, getOrderLimits, newNodes, repairer.multiplierOptimalThreshold)
	if err != nil {
		return false, nil, nil, nil, orderLimitFailureError.New("could not create PUT_REPAIR order limits: %w", err)
	}

	jobID, err := uuid.New()
	if err != nil {
		return false, nil, nil, nil, Error.New("failed to generate uuid: %v", err)
	}

	jobDefinition = &internalpb.RepairJobDefinition{
		JobId:             jobID[:],
		GetOrders:         getOrderLimits,
		PrivateKeyForGet:  getPrivateKey.Bytes(),
		PutOrders:         putLimits,
		PrivateKeyForPut:  putPrivateKey.Bytes(),
		Redundancy:        pointer.GetRemote().GetRedundancy(),
		SegmentSize:       int64(segment.EncryptedSize),
		DesiredPieceCount: int32(minSuccessfulNeeded),
		ExpirationTime:    time.Time{},
	}
	return false, jobDefinition, pointer, healthyPieceNums, nil
}

// PerformRepairJob performs a repair job: fetching pieces from the supplied
// GET nodes and storing new pieces to supplied PUT nodes.
func PerformRepairJob(ctx context.Context, ec *ECRepairer, jobDefinition *internalpb.RepairJobDefinition) (result internalpb.RepairJobResult) {
	result.JobId = jobDefinition.JobId

	if repairer.OnTestingPiecesReportHook != nil {
		repairer.OnTestingPiecesReportHook(piecesReport)
	}

	getPrivateKey, err := storj.PiecePrivateKeyFromBytes(jobDefinition.PrivateKeyForGet)
	if err != nil {
		result.ReconstructError = fmt.Sprintf("failed to unmarshal private key for GET: %v", err)
		return result
	}
	putPrivateKey, err := storj.PiecePrivateKeyFromBytes(jobDefinition.PrivateKeyForPut)
	if err != nil {
		result.ReconstructError = fmt.Sprintf("failed to unmarshal private key for PUT: %v", err)
		return result
	}
	redundancy, err := eestream.NewRedundancyStrategyFromProto(jobDefinition.Redundancy)
	if err != nil {
		result.ReconstructError = fmt.Sprintf("invalid redundancy strategy: %v", err)
		return result
	}

	if err != nil {
		// If the context was closed during the Get phase, it will appear here as though
		// we just failed to download enough pieces to reconstruct the segment. Check for
		// a closed context before doing any further error processing.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result
		}
		// If Get failed because of input validation, then it will keep failing. But if it
		// gave us irreparableError, then we failed to download enough pieces and must try
		// to wait for nodes to come back online.
		var irreparableErr *irreparableError
		if errors.As(err, &irreparableErr) {
			result.IrreparablePiecesRetrieved = irreparableErr.piecesAvailable
			result.ReconstructError = irreparableErr.Error()
			return result
		}
		result.ReconstructError = err.Error()
		return result
	}
	defer func() { err = errs.Combine(err, segmentReader.Close()) }()

	// Upload the repaired pieces
	timeout := jobDefinition.ExpirationTime.Sub(time.Now())
	healthyCount := len(jobDefinition.GetOrders) - len(failedPieces)
	numNeeded := int(jobDefinition.DesiredPieceCount) - healthyCount
	_, hashes, err := ec.Repair(ctx, jobDefinition.PutOrders, putPrivateKey, redundancy, segmentReader, timeout, path, numNeeded)
	if err != nil {
		result.StoreError = err.Error()
		return result
	}

	result.NewPiecesStored = hashes
	result.DeletePieceNums = failedPieces
	result.PutOrders = jobDefinition.PutOrders
	return result
}

func (repairer *SegmentRepairer) ProcessRepairResult(ctx context.Context, path storj.Path, pointer *pb.Pointer, healthyPieces []int32, repairResult internalpb.RepairJobResult) (shouldDelete bool, err error) {
	// Populate node IDs that failed piece hashes verification
	var failedNodeIDs storj.NodeIDList
	for _, pieceNum := range repairResult.DeletePieceNums {
		failedNodeIDs = append(failedNodeIDs, repairResult.PutOrders[pieceNum].Limit.StorageNodeId)
	}

	pieceSize := eestream.CalcPieceSize(int64(segment.EncryptedSize), redundancy)
	var bytesRepaired int64

	stats.repairTooManyNodesFailed.Mark(0)
	// Check if segment has been altered
	checkSegmentError := repairer.checkIfSegmentAltered(ctx, segment)
	if checkSegmentError != nil {
		if segmentDeletedError.Has(checkSegmentError) {
			// mon.Meter("segment_deleted_during_repair").Mark(1) //mon:locked
			repairer.log.Debug("segment deleted during Repair")
			return true, nil
		}
		if segmentModifiedError.Has(checkSegmentError) {
			// mon.Meter("segment_modified_during_repair").Mark(1) //mon:locked
			repairer.log.Debug("segment modified during Repair")
			return true, nil
		}
		return false, segmentVerificationError.Wrap(checkSegmentError)
	}

	if len(piecesReport.Contained) > 0 {
		repairer.log.Debug("unexpected contained pieces during repair", zap.Int("count", len(piecesReport.Contained)))
	}

	if repairResult.IrreparablePiecesRetrieved > 0 {
		mon.Meter("repair_too_many_nodes_failed").Mark(1) //mon:locked
		stats.repairTooManyNodesFailed.Mark(1)
		repairer.log.Warn("irreparable segment",
			zap.String("StreamID", queueSegment.StreamID.String()),
			zap.Uint64("Position", queueSegment.Position.Encode()),
			zap.Int32("piecesAvailable", irreparableErr.piecesAvailable),
			zap.Int32("piecesRequired", irreparableErr.piecesRequired),
			zap.Error(errs.Combine(irreparableErr.errlist...)),
		)
		return true, &irreparableError{
			path:            path,
			piecesAvailable: repairResult.IrreparablePiecesRetrieved,
			piecesRequired:  pointer.Remote.Redundancy.MinReq,
			segmentInfo:     pointer,
		}
	}
	// ensure we get values, even if only zero values, so that redash can have an alert based on this
	mon.Meter("repair_too_many_nodes_failed").Mark(0) //mon:locked

	if repairResult.ReconstructError != "" {
		return true, repairReconstructError.New("segment could not be reconstructed: %w", err)
	}

	// only report audit result when segment can be successfully downloaded
	var report audit.Report
	for _, piece := range piecesReport.Successful {
		report.Successes = append(report.Successes, piece.StorageNode)
	}
	for _, piece := range piecesReport.Failed {
		report.Fails = append(report.Fails, piece.StorageNode)
	}
	for _, piece := range piecesReport.Offline {
		report.Offlines = append(report.Offlines, piece.StorageNode)
	}
	for _, piece := range piecesReport.Unknown {
		report.Unknown = append(report.Unknown, piece.StorageNode)
	}
	_, reportErr := repairer.reporter.RecordAudits(ctx, report)
	if reportErr != nil {
		// failed updates should not affect repair, therefore we will not return the error
		repairer.log.Debug("failed to record audit", zap.Error(reportErr))
	}

	if len(repairResult.NewPiecesStored) != len(repairResult.PutOrders) {
		return false, repairResultError.New("%d NewPiecesStored, but %d PutOrders", len(repairResult.NewPiecesStored), len(repairResult.PutOrders))
	}
	// Add the successfully uploaded pieces to repairedPieces
	var repairedPieces metabase.Pieces
	repairedMap := make(map[uint16]bool)
	for i, pieceHash := range repairResult.NewPiecesStored {
		if pieceHash == nil {
			continue
		}
		bytesRepaired += pieceSize
		piece := metabase.Piece{
			Number:      uint16(i),
			StorageNode: repairResult.PutOrders[i].Limit.StorageNodeId,
		}
		repairedPieces = append(repairedPieces, piece)
		repairedMap[uint16(i)] = true
	}

	mon.Meter("repair_bytes_uploaded").Mark64(bytesRepaired) //mon:locked

	healthyPieceSet := sliceToSet(healthyPieces)
	failedPieceSet := sliceToSet(repairResult.DeletePieceNums)
	healthyAfterRepair := int32(len(healthyPieceSet) - len(failedPieceSet) + len(repairedMap))
	switch {
	case healthyAfterRepair <= int(segment.Redundancy.RepairShares):
		// Important: this indicates a failure to PUT enough pieces to the network to pass
		// the repair threshold, and _not_ a failure to reconstruct the segment. But we
		// put at least one piece, else ec.Repair() would have returned an error. So the
		// repair "succeeded" in that the segment is now healthier than it was, but it is
		// not as healthy as we want it to be.
		mon.Meter("repair_failed").Mark(1) //mon:locked
		stats.repairFailed.Mark(1)
	case healthyAfterRepair < int(segment.Redundancy.OptimalShares):
		mon.Meter("repair_partial").Mark(1) //mon:locked
		stats.repairPartial.Mark(1)
	default:
		mon.Meter("repair_success").Mark(1) //mon:locked
		stats.repairSuccess.Mark(1)
	}

	healthyRatioAfterRepair := 0.0
	if segment.Redundancy.TotalShares != 0 {
		healthyRatioAfterRepair = float64(healthyAfterRepair) / float64(segment.Redundancy.TotalShares)
	}

	mon.FloatVal("healthy_ratio_after_repair").Observe(healthyRatioAfterRepair) //mon:locked
	stats.healthyRatioAfterRepair.Observe(healthyRatioAfterRepair)

	var toRemove metabase.Pieces
	// if full repair, remove all unhealthy pieces. Otherwise, leave unrepaired and
	// unhealthy pieces in the pointer.
	removeAllUnhealthy := false
	if healthyAfterRepair >= segment.Redundancy.OptimalShares {
		removeAllUnhealthy = true
	}
	for _, piece := range pointer.Remote.RemotePieces {
		// remove pieces from pointer if:
		//      the node failed its hash verification (failedPieceSet)
		//   OR the piece was repaired to a new node (repairedMap)
		//   OR we're removing _all_ unhealthy pieces and the piece is unhealthy
		if failedPieceSet[piece.PieceNum] ||
			repairedMap[piece.PieceNum] ||
			(removeAllUnhealthy && !healthyPieceSet[piece.PieceNum]) {
			toRemove = append(toRemove, piece)
		}
	}

	newPieces, err := segment.Pieces.Update(repairedPieces, toRemove)
	if err != nil {
		return false, repairPutError.Wrap(err)
	}

	err = repairer.metabase.UpdateSegmentPieces(ctx, metabase.UpdateSegmentPieces{
		StreamID: segment.StreamID,
		Position: segment.Position,

		OldPieces:     segment.Pieces,
		NewRedundancy: segment.Redundancy,
		NewPieces:     newPieces,

		NewRepairedAt: time.Now(),
	})
	if err != nil {
		return false, metainfoPutError.Wrap(err)
	}

	repairedAt := time.Time{}
	if segment.RepairedAt != nil {
		repairedAt = *segment.RepairedAt
	}

	var segmentAge time.Duration
	if segment.CreatedAt.Before(repairedAt) {
		segmentAge = time.Since(repairedAt)
	} else {
		segmentAge = time.Since(segment.CreatedAt)
	}

	// TODO what to do with RepairCount
	var repairCount int64
	// pointer.RepairCount++

	mon.IntVal("segment_time_until_repair").Observe(int64(segmentAge.Seconds())) //mon:locked
	stats.segmentTimeUntilRepair.Observe(int64(segmentAge.Seconds()))
	mon.IntVal("segment_repair_count").Observe(repairCount) //mon:locked
	stats.segmentRepairCount.Observe(repairCount)

	return true, nil
}

// checkIfSegmentAltered checks if oldSegment has been altered since it was selected for audit.
func (repairer *SegmentRepairer) checkIfSegmentAltered(ctx context.Context, oldSegment metabase.Segment) (err error) {
	defer mon.Task()(&ctx)(&err)

	if repairer.OnTestingCheckSegmentAlteredHook != nil {
		repairer.OnTestingCheckSegmentAlteredHook()
	}

	newSegment, err := repairer.metabase.GetSegmentByPosition(ctx, metabase.GetSegmentByPosition{
		StreamID: oldSegment.StreamID,
		Position: oldSegment.Position,
	})
	if err != nil {
		if metabase.ErrSegmentNotFound.Has(err) {
			return segmentDeletedError.New("StreamID: %q Position: %d", oldSegment.StreamID.String(), oldSegment.Position.Encode())
		}
		return err
	}

	if !oldSegment.Pieces.Equal(newSegment.Pieces) {
		return segmentModifiedError.New("StreamID: %q Position: %d", oldSegment.StreamID.String(), oldSegment.Position.Encode())
	}
	return nil
}

func (repairer *SegmentRepairer) getStatsByRS(redundancy *pb.RedundancyScheme) *stats {
	rsString := getRSString(repairer.loadRedundancy(redundancy))
	return repairer.statsCollector.getStatsByRS(rsString)
}

func (repairer *SegmentRepairer) loadRedundancy(redundancy *pb.RedundancyScheme) (int, int, int, int) {
	repair := int(redundancy.RepairThreshold)
	overrideValue := repairer.repairOverrides.GetOverrideValuePB(redundancy)
	if overrideValue != 0 {
		repair = int(overrideValue)
	}
	return int(redundancy.MinReq), repair, int(redundancy.SuccessThreshold), int(redundancy.Total)
}

// SetNow allows tests to have the server act as if the current time is whatever they want.
func (repairer *SegmentRepairer) SetNow(nowFn func() time.Time) {
	repairer.nowFn = nowFn
}

// sliceToSet converts the given slice to a set.
func sliceToSet(slice []uint16) map[uint16]bool {
	set := make(map[uint16]bool, len(slice))
	for _, value := range slice {
		set[value] = true
	}
	return set
}
