/*
 * Copyright (c) 2018 VMware, Inc.
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy of this software and
 * associated documentation files (the "Software"), to deal in the Software without restriction, including
 * without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is furnished to do
 * so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all copies or substantial
 * portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT
 * NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
 * IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
 * WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
 * SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
 */

// Package worker
// The implementation is derived from https://github.com/patrobinson/gokini
//
// Copyright 2018 Patrick robinson.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
package worker

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/kinesis/types"

	chk "github.com/vmware/vmware-go-kcl-v2/clientlibrary/checkpoint"
	kcl "github.com/vmware/vmware-go-kcl-v2/clientlibrary/interfaces"
	"github.com/vmware/vmware-go-kcl-v2/clientlibrary/metrics"
)

const (
	MaxReadTransactionsPerSecond = 5
)

// PollingShardConsumer is responsible for polling data records from a (specified) shard.
// Note: PollingShardConsumer only deal with one shard.
type PollingShardConsumer struct {
	commonShardConsumer
	streamName string
	stop       *chan struct{}
	consumerID string
	mService   metrics.MonitoringService
}

func (sc *PollingShardConsumer) getShardIterator() (*string, error) {
	startPosition, err := sc.getStartingPosition()
	if err != nil {
		return nil, err
	}

	shardIterArgs := &kinesis.GetShardIteratorInput{
		ShardId:                &sc.shard.ID,
		ShardIteratorType:      startPosition.Type,
		StartingSequenceNumber: startPosition.SequenceNumber,
		Timestamp:              startPosition.Timestamp,
		StreamName:             &sc.streamName,
	}

	iterResp, err := sc.kc.GetShardIterator(context.TODO(), shardIterArgs)
	if err != nil {
		return nil, err
	}

	return iterResp.ShardIterator, nil
}

// getRecords continuously poll one shard for data record
// Precondition: it currently has the lease on the shard.
func (sc *PollingShardConsumer) getRecords() error {
	defer sc.releaseLease(sc.shard.ID)

	log := sc.kclConfig.Logger

	// If the shard is child shard, need to wait until the parent finished.
	if err := sc.waitOnParentShard(); err != nil {
		// If parent shard has been deleted by Kinesis system already, just ignore the error.
		if err != chk.ErrSequenceIDNotFound {
			log.Errorf("Error in waiting for parent shard: %v to finish. Error: %+v", sc.shard.ParentShardId, err)
			return err
		}
	}

	shardIterator, err := sc.getShardIterator()
	if err != nil {
		log.Errorf("Unable to get shard iterator for %s: %v", sc.shard.ID, err)
		return err
	}

	// Start processing events and notify record processor on shard and starting checkpoint
	input := &kcl.InitializationInput{
		ShardId:                sc.shard.ID,
		ExtendedSequenceNumber: &kcl.ExtendedSequenceNumber{SequenceNumber: aws.String(sc.shard.GetCheckpoint())},
	}
	sc.recordProcessor.Initialize(input)

	recordCheckpointer := NewRecordProcessorCheckpoint(sc.shard, sc.checkpointer)
	retriedErrors := 0
	transactionNum := 0
	var firstTransactionTime time.Time

	for {
		if time.Now().UTC().After(sc.shard.GetLeaseTimeout().Add(-time.Duration(sc.kclConfig.LeaseRefreshPeriodMillis) * time.Millisecond)) {
			log.Debugf("Refreshing lease on shard: %s for worker: %s", sc.shard.ID, sc.consumerID)
			err = sc.checkpointer.GetLease(sc.shard, sc.consumerID)
			if err != nil {
				if errors.As(err, &chk.ErrLeaseNotAcquired{}) {
					log.Warnf("Failed in acquiring lease on shard: %s for worker: %s", sc.shard.ID, sc.consumerID)
					return nil
				}
				// log and return error
				log.Errorf("Error in refreshing lease on shard: %s for worker: %s. Error: %+v",
					sc.shard.ID, sc.consumerID, err)
				return err
			}
			// log metric for renewed lease for worker
			sc.mService.LeaseRenewed(sc.shard.ID)
		}

		getRecordsStartTime := time.Now()

		log.Debugf("Trying to read %d record from iterator: %v", sc.kclConfig.MaxRecords, aws.ToString(shardIterator))
		getRecordsArgs := &kinesis.GetRecordsInput{
			Limit:         aws.Int32(int32(sc.kclConfig.MaxRecords)),
			ShardIterator: shardIterator,
		}

		// Each shard can support up to five read transactions per second.
		if transactionNum > MaxReadTransactionsPerSecond {
			transactionNum = 0
			timeDiff := time.Since(firstTransactionTime)
			if timeDiff < time.Second {
				time.Sleep(timeDiff)
			}
		}

		// Get records from stream and retry as needed
		// Each read transaction can provide up to 10,000 records with an upper quota of 10 MB per transaction.
		// ref: https://docs.aws.amazon.com/streams/latest/dev/service-sizes-and-limits.html
		getResp, err := sc.kc.GetRecords(context.TODO(), getRecordsArgs)
		getRecordsTransactionTime := time.Now()
		if err != nil {
			//aws-sdk-go-v2 https://github.com/aws/aws-sdk-go-v2/blob/main/CHANGELOG.md#error-handling
			var throughputExceededErr *types.ProvisionedThroughputExceededException
			var kmsThrottlingErr *types.KMSThrottlingException
			if errors.As(err, &throughputExceededErr) {
				retriedErrors++
				if retriedErrors > sc.kclConfig.MaxRetryCount {
					log.Errorf("message", "reached max retry count getting records from shard",
						"shardId", sc.shard.ID,
						"retryCount", retriedErrors,
						"error", err)
					return err
				}
				// If there is insufficient provisioned throughput on the stream,
				// subsequent calls made within the next 1 second throw ProvisionedThroughputExceededException.
				// ref: https://docs.aws.amazon.com/streams/latest/dev/service-sizes-and-limits.html
				waitTime := time.Since(getRecordsTransactionTime)
				if waitTime < time.Second {
					time.Sleep(time.Second - waitTime)
				}
				continue
			}
			if errors.As(err, &kmsThrottlingErr) {
				log.Errorf("Error getting records from shard %v: %+v", sc.shard.ID, err)
				retriedErrors++
				// Greater than MaxRetryCount so we get the last retry
				if retriedErrors > sc.kclConfig.MaxRetryCount {
					log.Errorf("message", "reached max retry count getting records from shard",
						"shardId", sc.shard.ID,
						"retryCount", retriedErrors,
						"error", err)
					return err
				}
				// exponential backoff
				// https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Programming.Errors.html#Programming.Errors.RetryAndBackoff
				time.Sleep(time.Duration(math.Exp2(float64(retriedErrors))*100) * time.Millisecond)
				continue
			}
			log.Errorf("Error getting records from Kinesis that cannot be retried: %+v Request: %s", err, getRecordsArgs)
			return err
		}

		// reset the retry count after success
		retriedErrors = 0

		// Add to number of getRecords successful transactions
		transactionNum++
		if transactionNum == 1 {
			firstTransactionTime = getRecordsTransactionTime
		}

		sc.processRecords(getRecordsStartTime, getResp.Records, getResp.MillisBehindLatest, recordCheckpointer)

		// The shard has been closed, so no new records can be read from it
		if getResp.NextShardIterator == nil {
			log.Infof("Shard %s closed", sc.shard.ID)
			shutdownInput := &kcl.ShutdownInput{ShutdownReason: kcl.TERMINATE, Checkpointer: recordCheckpointer}
			sc.recordProcessor.Shutdown(shutdownInput)
			return nil
		}
		shardIterator = getResp.NextShardIterator

		// Idle between each read, the user is responsible for checkpoint the progress
		// This value is only used when no records are returned; if records are returned, it should immediately
		// retrieve the next set of records.
		if len(getResp.Records) == 0 && aws.ToInt64(getResp.MillisBehindLatest) < int64(sc.kclConfig.IdleTimeBetweenReadsInMillis) {
			time.Sleep(time.Duration(sc.kclConfig.IdleTimeBetweenReadsInMillis) * time.Millisecond)
		}

		select {
		case <-*sc.stop:
			shutdownInput := &kcl.ShutdownInput{ShutdownReason: kcl.REQUESTED, Checkpointer: recordCheckpointer}
			sc.recordProcessor.Shutdown(shutdownInput)
			return nil
		default:
		}
	}
}
