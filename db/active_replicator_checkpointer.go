/*
Copyright 2020-Present Couchbase, Inc.

Use of this software is governed by the Business Source License included in
the file licenses/BSL-Couchbase.txt.  As of the Change Date specified in that
file, in accordance with the Business Source License, use of this software will
be governed by the Apache License, Version 2.0, included in the file
licenses/APL2.txt.
*/

package db

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/couchbase/go-blip"
	"github.com/couchbase/sync_gateway/base"
)

const defaultExpectedSeqCompactionThreshold = 100

// Checkpointer implements replicator checkpointing, by keeping two lists of sequences. Those which we expect to be processing revs for (either push or pull), and a map for those which we have done so on.
// Periodically (based on a time interval), these two lists are used to calculate the highest sequence number which we've not had a gap for yet, and send a SetCheckpoint message for this sequence.
type Checkpointer struct {
	clientID           string
	configHash         string
	blipSender         *blip.Sender
	dataStore          base.DataStore // dataStore is where the checkpoints are stored
	localDocExpirySecs int
	checkpointInterval time.Duration
	statusCallback     statusFunc // callback to retrieve status for associated replication
	// lock guards the expectedSeqs slice, and processedSeqs map
	lock sync.Mutex
	// expectedSeqs is an ordered list of sequence IDs we expect to process revs for
	expectedSeqs []SequenceID
	// processedSeqs is a map of sequence IDs we've processed revs for
	processedSeqs map[SequenceID]struct{}
	// idAndRevLookup is a temporary map of DocID/RevID pair to sequence number,
	// to handle cases where we receive a message that doesn't contain a sequence.
	idAndRevLookup map[IDAndRev]SequenceID
	// ctx is used to stop the checkpointer goroutine
	ctx context.Context
	// lastRemoteCheckpointRevID is the last known remote checkpoint RevID.
	lastRemoteCheckpointRevID string
	// lastLocalCheckpointRevID is the last known local checkpoint RevID.
	lastLocalCheckpointRevID string
	// lastCheckpointSeq is the last checkpointed sequence
	lastCheckpointSeq SequenceID
	// collectionIdx is the GetCollections index of the collection we're checkpointing for
	collectionIdx *int

	// expectedSeqCompactionThreshold is the number of expected sequences that we'll tolerate before considering compacting away already processed sequences
	// time vs. space complexity tradeoff, since we need to iterate over the expectedSeqs slice to compact it
	expectedSeqCompactionThreshold int

	stats CheckpointerStats

	// closeWg waits for the time-based checkpointer goroutine to finish.
	closeWg sync.WaitGroup
}

type statusFunc func(lastSeq string) *ReplicationStatus

type CheckpointerStats struct {
	ExpectedSequenceCount           int64
	ExpectedSequenceLen             *base.SgwIntStat
	ExpectedSequenceLenPostCleanup  *base.SgwIntStat
	ProcessedSequenceCount          int64
	ProcessedSequenceLen            *base.SgwIntStat
	ProcessedSequenceLenPostCleanup *base.SgwIntStat
	AlreadyKnownSequenceCount       int64
	SetCheckpointCount              int64
	GetCheckpointHitCount           int64
	GetCheckpointMissCount          int64
}

func NewCheckpointer(ctx context.Context, dataStore base.DataStore, clientID string, configHash string, blipSender *blip.Sender, replicatorConfig *ActiveReplicatorConfig, statusCallback statusFunc, collectionIdx *int) *Checkpointer {
	return &Checkpointer{
		clientID:           clientID,
		configHash:         configHash,
		blipSender:         blipSender,
		dataStore:          dataStore,
		localDocExpirySecs: int(replicatorConfig.ActiveDB.Options.LocalDocExpirySecs),
		expectedSeqs:       make([]SequenceID, 0),
		processedSeqs:      make(map[SequenceID]struct{}),
		idAndRevLookup:     make(map[IDAndRev]SequenceID),
		checkpointInterval: replicatorConfig.CheckpointInterval,
		ctx:                ctx,
		stats: CheckpointerStats{
			ProcessedSequenceLen:            replicatorConfig.ReplicationStatsMap.ProcessedSequenceLen,
			ProcessedSequenceLenPostCleanup: replicatorConfig.ReplicationStatsMap.ProcessedSequenceLenPostCleanup,
			ExpectedSequenceLen:             replicatorConfig.ReplicationStatsMap.ExpectedSequenceLen,
			ExpectedSequenceLenPostCleanup:  replicatorConfig.ReplicationStatsMap.ExpectedSequenceLenPostCleanup,
		},
		statusCallback:                 statusCallback,
		expectedSeqCompactionThreshold: defaultExpectedSeqCompactionThreshold,
		collectionIdx:                  collectionIdx,
	}
}

func (c *Checkpointer) AddAlreadyKnownSeq(seq ...SequenceID) {
	select {
	case <-c.ctx.Done():
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside AddAlreadyKnownSeq and context has been cancelled")
		// replicator already closed, bail out of checkpointing work
		return
	default:
	}

	c.lock.Lock()
	c.expectedSeqs = append(c.expectedSeqs, seq...)
	for _, seq := range seq {
		c.processedSeqs[seq] = struct{}{}
	}
	c.stats.AlreadyKnownSequenceCount += int64(len(seq))
	c.lock.Unlock()
}

func (c *Checkpointer) AddProcessedSeq(seq SequenceID) {
	select {
	case <-c.ctx.Done():
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside AddProcessedSeq and context has been cancelled")
		// replicator already closed, bail out of checkpointing work
		return
	default:
	}

	c.lock.Lock()
	c.processedSeqs[seq] = struct{}{}
	c.stats.ProcessedSequenceCount++
	c.lock.Unlock()
}

func (c *Checkpointer) AddProcessedSeqIDAndRev(seq *SequenceID, idAndRev IDAndRev) {
	select {
	case <-c.ctx.Done():
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside AddProcessedSeqIDAndRev and context has been cancelled")
		// replicator already closed, bail out of checkpointing work
		return
	default:
	}

	c.lock.Lock()

	if seq == nil {
		foundSeq, ok := c.idAndRevLookup[idAndRev]
		if !ok {
			base.WarnfCtx(c.ctx, "Unable to find matching sequence for %q / %q", base.UD(idAndRev.DocID), idAndRev.RevID)
		}
		seq = &foundSeq
	}
	// should remove entry in the map even if we have a seq available
	delete(c.idAndRevLookup, idAndRev)

	c.processedSeqs[*seq] = struct{}{}
	c.stats.ProcessedSequenceCount++

	c.lock.Unlock()
}

func (c *Checkpointer) AddExpectedSeqs(seqs ...SequenceID) {
	if len(seqs) == 0 {
		// nothing to do
		return
	}

	select {
	case <-c.ctx.Done():
		// replicator already closed, bail out of checkpointing work
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside AddExpectedSeqs and context has been cancelled")
		return
	default:
	}

	c.lock.Lock()
	c.expectedSeqs = append(c.expectedSeqs, seqs...)
	c.stats.ExpectedSequenceCount += int64(len(seqs))
	c.lock.Unlock()
}

func (c *Checkpointer) AddExpectedSeqIDAndRevs(seqs map[IDAndRev]SequenceID) {
	if len(seqs) == 0 {
		// nothing to do
		return
	}

	select {
	case <-c.ctx.Done():
		// replicator already closed, bail out of checkpointing work
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside AddExpectedSeqIDAndRevs and context has been cancelled")
		return
	default:
	}

	c.lock.Lock()
	for idAndRev, seq := range seqs {
		c.idAndRevLookup[idAndRev] = seq
		c.expectedSeqs = append(c.expectedSeqs, seq)
	}
	c.stats.ExpectedSequenceCount += int64(len(seqs))
	c.lock.Unlock()
}

func (c *Checkpointer) Start() {
	// Start a time-based checkpointer goroutine
	if c.checkpointInterval > 0 {
		c.closeWg.Add(1)
		go func() {
			defer c.closeWg.Done()
			ticker := time.NewTicker(c.checkpointInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					base.TracefCtx(c.ctx, base.KeyReplicate, "calling checkpoint now. context is not cancelled here")
					c.CheckpointNow()
				case <-c.ctx.Done():
					base.DebugfCtx(c.ctx, base.KeyReplicate, "checkpointer goroutine stopped")
					return
				}
			}
		}()
	}
}

// CheckpointNow forces the checkpointer to send a checkpoint, and blocks until it has finished.
func (c *Checkpointer) CheckpointNow() {
	if c == nil {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	// Retrieve status after obtaining the lock to ensure
	status := c.statusCallback(c._calculateSafeProcessedSeq().String())

	base.TracefCtx(c.ctx, base.KeyReplicate, "checkpointer: running")

	seq := c._updateCheckpointLists()
	if seq == nil {
		return
	}

	base.InfofCtx(c.ctx, base.KeyReplicate, "checkpointer: calculated seq: %v", seq)
	err := c._setCheckpoints(seq, status)
	if err != nil {
		base.WarnfCtx(c.ctx, "couldn't set checkpoints: %v", err)
	}
}

// Stats returns a copy of the checkpointer stats. Intended for test use - non-test usage may have
// performance implications associated with locking
func (c *Checkpointer) Stats() CheckpointerStats {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.stats
}

// _updateCheckpointLists determines the highest checkpointable sequence, and trims the processedSeqs/expectedSeqs lists up to this point.
// We will also remove all but the last processed sequence as we know we're able to checkpoint safely up to that point without leaving any intermediate sequence numbers around.
func (c *Checkpointer) _updateCheckpointLists() (safeSeq *SequenceID) {
	base.TracefCtx(c.ctx, base.KeyReplicate, "checkpointer: _updateCheckpointLists(expectedSeqs: %v, processedSeqs: %v)", c.expectedSeqs, c.processedSeqs)
	base.TracefCtx(c.ctx, base.KeyReplicate, "Inside update checkpoint lists")

	c.stats.ProcessedSequenceLen.Set(int64(len(c.processedSeqs)))
	c.stats.ExpectedSequenceLen.Set(int64(len(c.expectedSeqs)))
	maxI := c._calculateSafeExpectedSeqsIdx()

	if maxI > -1 {
		seq := c.expectedSeqs[maxI]

		// removes to-be checkpointed sequences from processedSeqs list
		for i := 0; i <= maxI; i++ {
			removeSeq := c.expectedSeqs[i]
			delete(c.processedSeqs, removeSeq)
			base.TracefCtx(c.ctx, base.KeyReplicate, "checkpointer: _updateCheckpointLists removed seq %v from processedSeqs map", removeSeq)
		}

		// trim expectedSeqs list from beginning up to first unprocessed seq
		c.expectedSeqs = c.expectedSeqs[maxI+1:]
		safeSeq = &seq
	}

	// if we have many remaining expectedSeqs, see if we can shrink the lists even more
	// compact contiguous blocks of sequences by keeping only the last processed sequence in both lists
	if len(c.expectedSeqs) > c.expectedSeqCompactionThreshold {
		// start at the one before the end of the list (since we know we need to retain that one anyway, if it's processed)
		for i := len(c.expectedSeqs) - 2; i >= 0; i-- {
			current := c.expectedSeqs[i]
			next := c.expectedSeqs[i+1]
			_, processedCurrent := c.processedSeqs[current]
			_, processedNext := c.processedSeqs[next]
			if processedCurrent && processedNext {
				// remove the current sequence from both sets, since we know we've also processed the next sequence and are able to checkpoint that
				delete(c.processedSeqs, current)
				c.expectedSeqs = append(c.expectedSeqs[:i], c.expectedSeqs[i+1:]...)
			}
		}
	}

	c.stats.ProcessedSequenceLenPostCleanup.Set(int64(len(c.processedSeqs)))
	c.stats.ExpectedSequenceLenPostCleanup.Set(int64(len(c.expectedSeqs)))
	return safeSeq
}

// _calculateSafeExpectedSeqsIdx returns an index into expectedSeqs which is safe to checkpoint.
// Returns -1 if no sequence in the list is able to be checkpointed.
func (c *Checkpointer) _calculateSafeExpectedSeqsIdx() int {
	safeIdx := -1

	sort.Slice(c.expectedSeqs, func(i, j int) bool {
		return c.expectedSeqs[i].Before(c.expectedSeqs[j])
	})

	// iterates over each (ordered) expected sequence and stops when we find the first sequence we've yet to process a rev message for
	for i, seq := range c.expectedSeqs {
		if _, ok := c.processedSeqs[seq]; !ok {
			break
		}
		safeIdx = i
	}

	return safeIdx
}

// calculateSafeProcessedSeq returns the sequence last processed that is able to be checkpointed, or the last checkpointed sequence.
func (c *Checkpointer) calculateSafeProcessedSeq() SequenceID {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c._calculateSafeProcessedSeq()
}

func (c *Checkpointer) _calculateSafeProcessedSeq() SequenceID {
	idx := c._calculateSafeExpectedSeqsIdx()
	if idx == -1 {
		return c.lastCheckpointSeq
	}

	return c.expectedSeqs[idx]
}

const (
	checkpointBodyRev     = "_rev"
	checkpointBodyLastSeq = "last_sequence"
	checkpointBodyHash    = "config_hash"
	checkpointBodyStatus  = "status"
)

type replicationCheckpoint struct {
	Rev        string             `json:"_rev"`
	ConfigHash string             `json:"config_hash"`
	LastSeq    string             `json:"last_sequence"`
	Status     *ReplicationStatus `json:"status,omitempty"`
}

// AsBody returns a Body representation of replicationCheckpoint for use with putSpecial
func (r *replicationCheckpoint) AsBody() Body {
	return Body{
		checkpointBodyRev:     r.Rev,
		checkpointBodyLastSeq: r.LastSeq,
		checkpointBodyHash:    r.ConfigHash,
		checkpointBodyStatus:  r.Status,
	}
}

// NewReplicationCheckpoint converts a revID and checkpoint body into a replicationCheckpoint
func NewReplicationCheckpoint(revID string, body []byte) (checkpoint *replicationCheckpoint, err error) {

	err = base.JSONUnmarshal(body, &checkpoint)
	if err != nil {
		return nil, err
	}
	checkpoint.Rev = revID
	return checkpoint, nil
}

func (r *replicationCheckpoint) Copy() *replicationCheckpoint {
	return &replicationCheckpoint{
		Rev:        r.Rev,
		LastSeq:    r.LastSeq,
		ConfigHash: r.ConfigHash,
	}
}

// fetchDefaultCollectionCheckpoints sets lastCheckpointSeq for the given Checkpointer by requesting various checkpoints on the local and remote.
// Various scenarios this function handles:
// - Matching checkpoints from local and remote. Use that sequence.
// - Both SGR2 checkpoints are missing, we'll start the replication from zero.
// - Mismatched config hashes, use a zero value for sequence, so the replication can restart.
// - Mismatched sequences, we'll pick the lower of the two, and attempt to roll back the higher checkpoint to that point.
func (c *Checkpointer) fetchDefaultCollectionCheckpoints() error {
	base.TracefCtx(c.ctx, base.KeyReplicate, "fetchDefaultCollectionCheckpoints()")

	localCheckpoint, err := c.getLocalCheckpoint()
	if err != nil {
		return err
	}

	base.DebugfCtx(c.ctx, base.KeyReplicate, "got local checkpoint: %v", localCheckpoint)
	c.lastLocalCheckpointRevID = localCheckpoint.Rev

	remoteCheckpoint, err := c.getRemoteCheckpoint()
	if err != nil {
		return err
	}
	base.DebugfCtx(c.ctx, base.KeyReplicate, "got remote checkpoint: %v", remoteCheckpoint)
	c.lastRemoteCheckpointRevID = remoteCheckpoint.Rev

	localSeq := localCheckpoint.LastSeq
	remoteSeq := remoteCheckpoint.LastSeq

	// If localSeq and remoteSeq match, we'll use localSeq as the value.
	checkpointSeq := localSeq

	// Determine the lowest sequence if they don't match
	if localSeq != remoteSeq {
		base.DebugfCtx(c.ctx, base.KeyReplicate, "sequences mismatched, finding lowest of %q %q", localSeq, remoteSeq)
		localSeqVal, err := parseIntegerSequenceID(localSeq)
		if err != nil {
			return err
		}

		remoteSeqVal, err := parseIntegerSequenceID(remoteSeq)
		if err != nil {
			return err
		}

		// roll local/remote checkpoint back to lowest of the two
		if remoteSeqVal.Before(localSeqVal) {
			checkpointSeq = remoteSeq
			newLocalCheckpoint := remoteCheckpoint.Copy()
			newLocalCheckpoint.Rev = c.lastLocalCheckpointRevID
			c.lastLocalCheckpointRevID, err = c.setLocalCheckpoint(newLocalCheckpoint)
			if err != nil {
				base.WarnfCtx(c.ctx, "Unable to roll back local checkpoint: %v", err)
			} else {
				base.InfofCtx(c.ctx, base.KeyReplicate, "Rolled back local checkpoint to remote: %v", remoteSeqVal)
			}
		} else {
			// checkpointSeq already set above for localSeq value
			newRemoteCheckpoint := localCheckpoint.Copy()
			newRemoteCheckpoint.Rev = c.lastRemoteCheckpointRevID
			c.lastRemoteCheckpointRevID, err = c.setRemoteCheckpoint(newRemoteCheckpoint)
			if err != nil {
				base.WarnfCtx(c.ctx, "Unable to roll back remote checkpoint: %v", err)
			} else {
				base.InfofCtx(c.ctx, base.KeyReplicate, "Rolled back remote checkpoint to local: %v", localSeqVal)
			}
		}
	}

	// If checkpoint hash has changed, reset checkpoint to zero. Shouldn't need to persist the updated checkpoints in
	// the case both get rolled back to zero, can wait for next persistence.
	if localCheckpoint.ConfigHash != c.configHash || remoteCheckpoint.ConfigHash != c.configHash {
		base.DebugfCtx(c.ctx, base.KeyReplicate, "replicator config changed, unable to use previous checkpoint")
		checkpointSeq = ""
	}

	var parsedCheckpointSeq SequenceID
	if checkpointSeq != "" {
		parsedCheckpointSeq, err = ParsePlainSequenceID(checkpointSeq)
		if err == nil {
			c.stats.GetCheckpointHitCount++
		} else {
			base.WarnfCtx(c.ctx, "couldn't parse checkpoint sequence %q, unable to use previous checkpoint: %v", checkpointSeq, err)
			c.stats.GetCheckpointMissCount++
		}
	} else {
		c.stats.GetCheckpointMissCount++
	}

	base.InfofCtx(c.ctx, base.KeyReplicate, "using checkpointed seq: %q", parsedCheckpointSeq.String())
	c.lastCheckpointSeq = parsedCheckpointSeq

	return nil
}

func (c *Checkpointer) _setCheckpoints(seq *SequenceID, status *ReplicationStatus) (err error) {
	seqStr := seq.String()
	base.TracefCtx(c.ctx, base.KeyReplicate, "setCheckpoints(%v)", seqStr)
	c.lastLocalCheckpointRevID, err = c.setLocalCheckpointWithRetry(
		&replicationCheckpoint{
			LastSeq:    seqStr,
			Rev:        c.lastLocalCheckpointRevID,
			ConfigHash: c.configHash,
			Status:     status,
		})
	if err != nil {
		return err
	}

	c.lastRemoteCheckpointRevID, err = c.setRemoteCheckpointWithRetry(
		&replicationCheckpoint{
			LastSeq:    seqStr,
			Rev:        c.lastRemoteCheckpointRevID,
			ConfigHash: c.configHash,
		})
	if err != nil {
		return err
	}

	c.lastCheckpointSeq = *seq
	c.stats.SetCheckpointCount++

	return nil
}

// getLocalCheckpoint returns the sequence and rev for the local checkpoint.
// if the checkpoint does not exist, returns empty sequence and rev.
func (c *Checkpointer) getLocalCheckpoint() (checkpoint *replicationCheckpoint, err error) {
	base.TracefCtx(c.ctx, base.KeyReplicate, "getLocalCheckpoint")

	checkpointBytes, err := getSpecialBytes(c.dataStore, DocTypeLocal, CheckpointDocIDPrefix+c.clientID, c.localDocExpirySecs)
	if err != nil {
		if !base.IsKeyNotFoundError(c.dataStore, err) {
			return &replicationCheckpoint{}, err
		}
		base.DebugfCtx(c.ctx, base.KeyReplicate, "couldn't find existing local checkpoint for client %q", c.clientID)
		return &replicationCheckpoint{}, nil
	}

	err = base.JSONUnmarshal(checkpointBytes, &checkpoint)
	return checkpoint, err
}

func (c *Checkpointer) setLocalCheckpoint(checkpoint *replicationCheckpoint) (newRev string, err error) {
	newRev, err = putSpecial(c.dataStore, DocTypeLocal, CheckpointDocIDPrefix+c.clientID, checkpoint.Rev, checkpoint.AsBody(), c.localDocExpirySecs)
	if err != nil {
		base.TracefCtx(c.ctx, base.KeyReplicate, "Error setting local checkpoint(%v): %v", checkpoint, err)
		return "", err
	}
	base.TracefCtx(c.ctx, base.KeyReplicate, "setLocalCheckpoint(%v)", checkpoint)
	return newRev, nil
}

// setLocalCheckpointWithRetry attempts to rewrite the checkpoint if the rev ID is mismatched, or the checkpoint has since been deleted.
func (c *Checkpointer) setLocalCheckpointWithRetry(checkpoint *replicationCheckpoint) (newRevID string, err error) {
	return c.setRetry(checkpoint,
		c.setLocalCheckpoint,
		c.getLocalCheckpoint,
	)
}

// resetLocalCheckpoint removes the local checkpoint to roll back the replication.
func resetLocalCheckpoint(dataStore base.DataStore, checkpointID string) error {
	key := RealSpecialDocID(DocTypeLocal, CheckpointDocIDPrefix+checkpointID)
	if err := dataStore.Delete(key); err != nil && !base.IsDocNotFoundError(err) {
		return err
	}
	return nil
}

// getRemoteCheckpoint returns the sequence and rev for the remote checkpoint.
// if the checkpoint does not exist, returns empty sequence and rev.
func (c *Checkpointer) getRemoteCheckpoint() (checkpoint *replicationCheckpoint, err error) {
	base.TracefCtx(c.ctx, base.KeyReplicate, "getRemoteCheckpoint")

	rq := GetSGR2CheckpointRequest{
		Client:        c.clientID,
		CollectionIdx: c.collectionIdx,
	}

	if err := rq.Send(c.blipSender); err != nil {
		return &replicationCheckpoint{}, err
	}

	resp, err := rq.Response()
	if err != nil {
		return &replicationCheckpoint{}, err
	}

	if resp == nil {
		base.DebugfCtx(c.ctx, base.KeyReplicate, "couldn't find existing remote checkpoint for client %q", c.clientID)
		return &replicationCheckpoint{}, nil
	}

	return NewReplicationCheckpoint(resp.RevID, resp.BodyBytes)
}

func (c *Checkpointer) setRemoteCheckpoint(checkpoint *replicationCheckpoint) (newRev string, err error) {

	base.TracefCtx(c.ctx, base.KeyReplicate, "setRemoteCheckpoint(%v)", checkpoint)

	checkpointBody := checkpoint.AsBody()
	rq := SetSGR2CheckpointRequest{
		Client:        c.clientID,
		Checkpoint:    checkpoint.AsBody(),
		CollectionIdx: c.collectionIdx,
	}

	parentRev, ok := checkpointBody[BodyRev].(string)
	if ok {
		rq.RevID = &parentRev
	}

	if err := rq.Send(c.blipSender); err != nil {
		return "", err
	}

	resp, err := rq.Response()
	if err != nil {
		return "", err
	}

	return resp.RevID, nil
}

// setRemoteCheckpointWithRetry attempts to rewrite the checkpoint if the rev ID is mismatched, or the checkpoint has since been deleted.
func (c *Checkpointer) setRemoteCheckpointWithRetry(checkpoint *replicationCheckpoint) (newRevID string, err error) {
	return c.setRetry(checkpoint,
		c.setRemoteCheckpoint,
		c.getRemoteCheckpoint,
	)
}

func (c *Checkpointer) getCounts() (expectedCount, processedCount int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return len(c.expectedSeqs), len(c.processedSeqs)
}

// waitForExpectedSequences waits for the expectedSeqs set to drain to zero.
// Intended to be used once the replication has been stopped, to wait for
// in-flight mutations to complete.
// Triggers immediate checkpointing if expectedCount == processedCount.
// Waits up to 10s, polling every 100ms.
func (c *Checkpointer) waitForExpectedSequences() error {
	waitCount := 0
	for waitCount < 100 {
		expectedCount, processedCount := c.getCounts()
		base.TracefCtx(c.ctx, base.KeyReplicate, "Inside waitForExpectedSequences loop expected %d and processed %d", expectedCount, processedCount)
		if expectedCount == 0 {
			return nil
		}
		if expectedCount == processedCount {
			c.CheckpointNow()
			// Doing an additional check here, instead of just 'continue',
			// in case of bugs that result in expectedCount==processedCount, but the
			// sets are not identical.  In that scenario, want to sleep before retrying
			updatedExpectedCount, _ := c.getCounts()
			base.TracefCtx(c.ctx, base.KeyReplicate, "Inside waitForExpectedSequences updated expected count %d", updatedExpectedCount)
			if updatedExpectedCount == 0 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
		waitCount++
	}
	return errors.New("checkpointer waitForExpectedSequences failed to complete after waiting 10s")
}

type setCheckpointFn func(checkpoint *replicationCheckpoint) (revID string, err error)
type getCheckpointFn func() (checkpoint *replicationCheckpoint, err error)

// setRetry is a retry loop for a setCheckpointFn, which will fetch a new RevID from a getCheckpointFn in the event of a write conflict.
func (c *Checkpointer) setRetry(checkpoint *replicationCheckpoint, setFn setCheckpointFn, getFn getCheckpointFn) (newRevID string, err error) {
	for numAttempts := 0; numAttempts < 10; numAttempts++ {
		newRevID, err = setFn(checkpoint)
		if err != nil {
			if strings.HasPrefix(err.Error(), "409") {
				existingCheckpoint, getErr := getFn()
				if getErr == nil {
					base.InfofCtx(c.ctx, base.KeyReplicate, "Revision mismatch in setCheckpoint - updated from %q to %q based on existing checkpoint, will retry", checkpoint.Rev, existingCheckpoint.Rev)
					checkpoint.Rev = existingCheckpoint.Rev
				} else {
					base.InfofCtx(c.ctx, base.KeyReplicate, "Revision mismatch in setCheckpoint, and unable to retrieve existing, will retry", getErr)
					// pause before falling through to retry, in case of temporary failure on getFn
					time.Sleep(time.Millisecond * 100)
				}
			} else if strings.HasPrefix(err.Error(), "404") {
				base.WarnfCtx(c.ctx, "checkpoint did not exist for attempted update - removing last known rev ID: %v", err)
				checkpoint.Rev = ""
			} else {
				base.WarnfCtx(c.ctx, "got unexpected error from setCheckpoint: %v", err)
			}
			continue
		}
		return newRevID, nil
	}
	return "", errors.New("failed to write checkpoint after 10 attempts")
}

// setLocalCheckpointStatus updates status in a replication checkpoint without a checkpointer.  Increments existing
// rev, preserves non-status fields (seq).  Requires lock to update checkpoint
func (c *Checkpointer) setLocalCheckpointStatus(status string, errorMessage string) {

	// getCheckpoint to obtain the current status
	checkpoint, err := c.getLocalCheckpoint()
	if err != nil {
		base.InfofCtx(c.ctx, base.KeyReplicate, "Unable to persist status update to checkpoint: %v", err)
	}
	if checkpoint == nil {
		checkpoint = &replicationCheckpoint{}
	}

	if checkpoint.Status == nil {
		checkpoint.Status = &ReplicationStatus{}
	}

	checkpoint.Status.Status = status
	checkpoint.Status.ErrorMessage = errorMessage
	base.TracefCtx(c.ctx, base.KeyReplicate, "setLocalCheckpoint(%v)", checkpoint)
	newRev, setErr := c.setLocalCheckpoint(checkpoint)
	if setErr != nil {
		base.WarnfCtx(c.ctx, "Unable to persist status in local checkpoint for %s, status not updated: %v", c.clientID, setErr)
	} else {
		base.TracefCtx(c.ctx, base.KeyReplicate, "setLocalCheckpointStatus successful for %s, newRev: %s: %+v %+v", c.clientID, newRev, checkpoint, checkpoint.Status)
	}
	c.lock.Lock()
	c.lastLocalCheckpointRevID = newRev
	c.lock.Unlock()
	return
}

func getLocalCheckpoint(dataStore base.DataStore, clientID string, localDocExpirySecs int) (*replicationCheckpoint, error) {
	base.TracefCtx(context.TODO(), base.KeyReplicate, "getLocalCheckpoint for %s", clientID)

	checkpointBytes, err := getSpecialBytes(dataStore, DocTypeLocal, CheckpointDocIDPrefix+clientID, localDocExpirySecs)
	if err != nil {
		if !base.IsKeyNotFoundError(dataStore, err) {
			return nil, err
		}
		base.DebugfCtx(context.TODO(), base.KeyReplicate, "couldn't find existing local checkpoint for ID %q", clientID)
		return nil, nil
	}
	var checkpoint *replicationCheckpoint
	err = base.JSONUnmarshal(checkpointBytes, &checkpoint)
	return checkpoint, err
}

// setLocalCheckpointStatus updates status in a replication checkpoint without a checkpointer.  Increments existing
// rev, preserves non-status fields (seq)
func setLocalCheckpointStatus(ctx context.Context, dataStore base.DataStore, clientID string, status string, errorMessage string, localDocExpirySecs int) {

	// getCheckpoint to obtain the current rev
	checkpoint, err := getLocalCheckpoint(dataStore, clientID, localDocExpirySecs)
	if err != nil {
		base.WarnfCtx(ctx, "Unable to retrieve local checkpoint for %s, status not updated", clientID)
		return
	}
	if checkpoint == nil {
		checkpoint = &replicationCheckpoint{}
	}

	if checkpoint.Status == nil {
		checkpoint.Status = &ReplicationStatus{}
	}

	checkpoint.Status.Status = status
	checkpoint.Status.ErrorMessage = errorMessage
	base.TracefCtx(ctx, base.KeyReplicate, "setLocalCheckpoint(%v)", checkpoint)
	newRev, putErr := putSpecial(dataStore, DocTypeLocal, CheckpointDocIDPrefix+clientID, checkpoint.Rev, checkpoint.AsBody(), localDocExpirySecs)
	if putErr != nil {
		base.WarnfCtx(ctx, "Unable to persist status in local checkpoint for %s, status not updated: %v", clientID, putErr)
	} else {
		base.TracefCtx(ctx, base.KeyReplicate, "setLocalCheckpointStatus successful for %s, newRev: %s: %+v %+v", clientID, newRev, checkpoint, checkpoint.Status)
	}
	return
}
