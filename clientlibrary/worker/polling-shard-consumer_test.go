/*
 * Copyright (c) 2023 VMware, Inc.
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

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestCallGetRecordsAPI(t *testing.T) {
	// basic happy path
	m1 := MockKinesisSubscriberGetter{}
	ret := kinesis.GetRecordsOutput{}
	m1.On("GetRecords", mock.Anything, mock.Anything, mock.Anything).Return(&ret, nil)
	psc := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m1},
	}
	gri := kinesis.GetRecordsInput{
		ShardIterator: aws.String("shard-iterator-01"),
	}
	out, err := psc.callGetRecordsAPI(&gri)
	assert.Nil(t, err)
	assert.Equal(t, &ret, out)
	m1.AssertExpectations(t)

	// check that localTPSExceededError is thrown when trying more than 5 TPS
	m2 := MockKinesisSubscriberGetter{}
	psc2 := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m2},
		callsLeft:           0,
	}
	rateLimitTimeSince = func(t time.Time) time.Duration {
		return 500 * time.Millisecond
	}
	out2, err2 := psc2.callGetRecordsAPI(&gri)
	assert.Nil(t, out2)
	assert.ErrorIs(t, err2, localTPSExceededError)
	m2.AssertExpectations(t)

	// check that getRecords is called normally in bytesRead = 0 case
	m3 := MockKinesisSubscriberGetter{}
	ret3 := kinesis.GetRecordsOutput{}
	m3.On("GetRecords", mock.Anything, mock.Anything, mock.Anything).Return(&ret3, nil)
	psc3 := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m3},
		callsLeft:           2,
		bytesRead:           0,
	}
	rateLimitTimeSince = func(t time.Time) time.Duration {
		return 2 * time.Second
	}
	out3, err3 := psc3.callGetRecordsAPI(&gri)
	assert.Nil(t, err3)
	assert.Equal(t, &ret3, out3)
	m3.AssertExpectations(t)

	// check that correct cool off period is taken for 10mb in 1 second
	testTime := time.Now()
	m4 := MockKinesisSubscriberGetter{}
	ret4 := kinesis.GetRecordsOutput{}
	m4.On("GetRecords", mock.Anything, mock.Anything, mock.Anything).Return(&ret4, nil)
	psc4 := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m4},
		callsLeft:           2,
		bytesRead:           MaxBytes,
		lastCheckTime:       testTime,
		remBytes:            MaxBytes,
	}
	rateLimitTimeSince = func(t time.Time) time.Duration {
		return 2 * time.Second
	}
	rateLimitTimeNow = func() time.Time {
		return testTime.Add(time.Second)
	}
	checkSleepVal := 0.0
	rateLimitSleep = func(d time.Duration) {
		checkSleepVal = d.Seconds()
	}
	out4, err4 := psc4.callGetRecordsAPI(&gri)
	assert.Nil(t, err4)
	assert.Equal(t, &ret4, out4)
	m4.AssertExpectations(t)
	if checkSleepVal != 5 {
		t.Errorf("Incorrect Cool Off Period: %v", checkSleepVal)
	}

	// check that no cool off period is taken for 6mb in 3 seconds
	testTime2 := time.Now()
	m5 := MockKinesisSubscriberGetter{}
	ret5 := kinesis.GetRecordsOutput{}
	m5.On("GetRecords", mock.Anything, mock.Anything, mock.Anything).Return(&ret5, nil)
	psc5 := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m5},
		callsLeft:           2,
		bytesRead:           MaxBytesPerSecond * 3,
		lastCheckTime:       testTime2,
		remBytes:            MaxBytes,
	}
	rateLimitTimeSince = func(t time.Time) time.Duration {
		return 3 * time.Second
	}
	rateLimitTimeNow = func() time.Time {
		return testTime2.Add(time.Second * 3)
	}
	checkSleepVal2 := 0.0
	rateLimitSleep = func(d time.Duration) {
		checkSleepVal2 = d.Seconds()
	}
	out5, err5 := psc5.callGetRecordsAPI(&gri)
	assert.Nil(t, err5)
	assert.Equal(t, &ret5, out5)
	m5.AssertExpectations(t)
	if checkSleepVal2 != 0 {
		t.Errorf("Incorrect Cool Off Period: %v", checkSleepVal2)
	}

	// check for correct cool off period with 8mb in .2 seconds with 6mb remaining
	testTime3 := time.Now()
	m6 := MockKinesisSubscriberGetter{}
	ret6 := kinesis.GetRecordsOutput{}
	m6.On("GetRecords", mock.Anything, mock.Anything, mock.Anything).Return(&ret6, nil)
	psc6 := PollingShardConsumer{
		commonShardConsumer: commonShardConsumer{kc: &m6},
		callsLeft:           2,
		bytesRead:           MaxBytesPerSecond * 4,
		lastCheckTime:       testTime3,
		remBytes:            MaxBytes * 3,
	}
	rateLimitTimeSince = func(t time.Time) time.Duration {
		return 3 * time.Second
	}
	rateLimitTimeNow = func() time.Time {
		return testTime3.Add(time.Second / 5)
	}
	checkSleepVal3 := 0.0
	rateLimitSleep = func(d time.Duration) {
		checkSleepVal3 = d.Seconds()
	}
	out6, err6 := psc6.callGetRecordsAPI(&gri)
	assert.Nil(t, err6)
	assert.Equal(t, &ret6, out6)
	m5.AssertExpectations(t)
	if checkSleepVal3 != 4 {
		t.Errorf("Incorrect Cool Off Period: %v", checkSleepVal3)
	}

	// restore original func
	rateLimitTimeNow = time.Now
	rateLimitTimeSince = time.Since
	rateLimitSleep = time.Sleep

}

type MockKinesisSubscriberGetter struct {
	mock.Mock
}

func (m *MockKinesisSubscriberGetter) GetRecords(ctx context.Context, params *kinesis.GetRecordsInput, optFns ...func(*kinesis.Options)) (*kinesis.GetRecordsOutput, error) {
	ret := m.Called(ctx, params, optFns)

	return ret.Get(0).(*kinesis.GetRecordsOutput), ret.Error(1)
}

func (m *MockKinesisSubscriberGetter) GetShardIterator(ctx context.Context, params *kinesis.GetShardIteratorInput, optFns ...func(*kinesis.Options)) (*kinesis.GetShardIteratorOutput, error) {
	return nil, nil
}

func (m *MockKinesisSubscriberGetter) SubscribeToShard(ctx context.Context, params *kinesis.SubscribeToShardInput, optFns ...func(*kinesis.Options)) (*kinesis.SubscribeToShardOutput, error) {
	return nil, nil
}
