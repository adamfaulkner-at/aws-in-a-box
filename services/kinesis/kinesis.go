package kinesis

import (
	"crypto/md5"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"aws-in-a-box/arn"
	"aws-in-a-box/awserrors"
)

var (
	uint128Max = big.NewInt(0).SetBytes([]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255})
)

func i64toA(i int64) string {
	return strconv.FormatInt(i, 10)
}

type Shard struct {
	Id string

	StartingHashKey big.Int
	EndingHashKey   big.Int

	StartingSequenceNumber int64
	EndingSequenceNumber   int64

	Records []APIRecord

	ConsumerChans map[chan *APISubscribeToShardEvent]struct{}
}

type Consumer struct {
	ARN               string
	Name              string
	StreamName        string
	CreationTimestamp int64

	ConsumerChansByShardId map[string]consumerSubscription
}

type consumerSubscription struct {
	CreationTime time.Time
	Chan         chan *APISubscribeToShardEvent
}

type Stream struct {
	// Immutable
	Name              string
	CreationTimestamp int64

	// Mutable
	Retention       time.Duration
	Shards          []*Shard
	Tags            map[string]string
	consumersByName map[string]*Consumer
}

type Kinesis struct {
	arnGenerator     arn.Generator
	defaultRetention time.Duration

	mu             sync.Mutex
	streams        map[string]*Stream
	consumersByARN map[string]*Consumer
}

func New(generator arn.Generator, defaultRetention time.Duration) *Kinesis {
	k := &Kinesis{
		arnGenerator:     generator,
		defaultRetention: defaultRetention,
		streams:          map[string]*Stream{},
		consumersByARN:   map[string]*Consumer{},
	}
	go func() {
		for {
			time.Sleep(k.defaultRetention / 2)
			k.enforceDuration()
		}
	}()
	return k
}

func (k *Kinesis) enforceDuration() {
	k.mu.Lock()
	defer k.mu.Unlock()

	now := time.Now()
	for _, stream := range k.streams {
		cutoff := now.Add(-stream.Retention).UnixNano()
		for _, shard := range stream.Shards {
			shard.Records = clip(shard.Records, cutoff)
		}
	}
}

func clip(records []APIRecord, cutoff int64) []APIRecord {
	retainStartingAt := len(records)
	for i, record := range records {
		if record.ApproximateArrivalTimestamp >= cutoff {
			retainStartingAt = i
			break
		}
	}
	if retainStartingAt == 0 {
		return records
	}

	recordsRemaining := len(records) - retainStartingAt
	copy(records, records[retainStartingAt:])
	return records[:recordsRemaining]
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_CreateStream.html
func (k *Kinesis) CreateStream(input CreateStreamInput) (*CreateStreamOutput, *awserrors.Error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if _, ok := k.streams[input.StreamName]; ok {
		return nil, XXXTodoException("Stream already exists")
	}

	stream := &Stream{
		Name:              input.StreamName,
		CreationTimestamp: time.Now().UnixNano(),
		consumersByName:   make(map[string]*Consumer),
		Tags:              make(map[string]string),
	}

	for tagName, tagValue := range input.Tags {
		stream.Tags[tagName] = tagValue
	}

	sequenceNumber := time.Now().UnixNano()

	step := big.NewInt(0).Div(uint128Max, big.NewInt(input.ShardCount))
	for i := int64(0); i < input.ShardCount; i++ {
		var start, end big.Int

		start.Mul(big.NewInt(i), step)
		end.Add(&start, step).Sub(&end, big.NewInt(1))
		if i == input.ShardCount-1 {
			end = *uint128Max
		}

		stream.Shards = append(stream.Shards, &Shard{
			// HACKY NAME??
			Id:                     input.StreamName + "@" + i64toA(i),
			StartingHashKey:        start,
			EndingHashKey:          end,
			StartingSequenceNumber: sequenceNumber,
			EndingSequenceNumber:   sequenceNumber,
			ConsumerChans:          make(map[chan *APISubscribeToShardEvent]struct{}),
		})
	}

	k.streams[input.StreamName] = stream
	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_DeleteStream.html
func (k *Kinesis) DeleteStream(input DeleteStreamInput) (*DeleteStreamOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if _, ok := k.streams[streamName]; !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	delete(k.streams, streamName)
	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_PutRecord.html
func (k *Kinesis) PutRecord(input PutRecordInput) (*PutRecordOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	fmt.Println("PutRecord", streamName)

	var hashKey big.Int
	if input.ExplicitHashKey != "" {
		hashKey.SetString(input.ExplicitHashKey, 10)
	} else {
		hash := md5.Sum([]byte(input.PartitionKey))
		hashKey.SetBytes(hash[:])
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, XXXTodoException("Stream does not exist")
	}

	for _, shard := range stream.Shards {
		if hashKey.Cmp(&shard.EndingHashKey) <= 0 && hashKey.Cmp(&shard.StartingHashKey) >= 0 {
			timestamp := time.Now().UnixNano()
			sequenceNumber := i64toA(timestamp)
			record := APIRecord{
				ApproximateArrivalTimestamp: timestamp,
				Data:                        input.Data,
				PartitionKey:                input.PartitionKey,
				SequenceNumber:              sequenceNumber,
			}
			shard.Records = append(shard.Records, record)

			for ch := range shard.ConsumerChans {
				ch <- &APISubscribeToShardEvent{
					Records:                    []APIRecord{record},
					ContinuationSequenceNumber: sequenceNumber,
				}
			}

			return &PutRecordOutput{
				ShardId:        shard.Id,
				SequenceNumber: sequenceNumber,
			}, nil
		}
	}

	panic("Could not find shard for record?")
}

func (k *Kinesis) lockedGetShard(streamName, shardId string) (*Shard, *awserrors.Error) {
	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	for _, shard := range stream.Shards {
		if shard.Id == shardId {
			return shard, nil
		}
	}

	return nil, awserrors.ResourceNotFoundException("Shard does not exist")
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_GetRecords.html
func (k *Kinesis) GetRecords(input GetRecordsInput) (*GetRecordsOutput, *awserrors.Error) {
	fmt.Println("GetRecords", input.ShardIterator)

	streamName, shardId, start, err := decodeShardIterator(input.ShardIterator)
	if err != nil {
		return nil, XXXTodoException(err.Error())
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	shard, awserr := k.lockedGetShard(streamName, shardId)
	if awserr != nil {
		return nil, awserr
	}

	output := &GetRecordsOutput{}
	var currIndex int
	for currIndex = start; currIndex < len(shard.Records); currIndex++ {
		output.Records = append(output.Records, shard.Records[currIndex])
		/*input.Limit -= 1
		if input.Limit <= 0 {
			break
		}*/
	}

	output.NextShardIterator = encodeShardIterator(streamName, shardId, currIndex)
	fmt.Println("READ RECORDS", input.ShardIterator, output.NextShardIterator)
	return output, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_GetShardIterator.html
func (k *Kinesis) GetShardIterator(input GetShardIteratorInput) (*GetShardIteratorOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	fmt.Println("GetShardIterator", streamName, input)

	output := &GetShardIteratorOutput{}
	switch input.ShardIteratorType {
	case "TRIM_HORIZON":
		output.ShardIterator = encodeShardIterator(streamName, input.ShardId, 0)
	case "LATEST":
		shard, err := k.lockedGetShard(streamName, input.ShardId)
		if err != nil {
			return nil, err
		}
		output.ShardIterator = encodeShardIterator(streamName, input.ShardId, len(shard.Records))
	case "AT_SEQUENCE_NUMBER":
		shard, err := k.lockedGetShard(streamName, input.ShardId)
		if err != nil {
			return nil, err
		}
		index := 0
		for i, record := range shard.Records {
			if record.SequenceNumber >= input.StartingSequenceNumber {
				index = i
				break
			}
		}
		output.ShardIterator = encodeShardIterator(streamName, input.ShardId, index)
	default:
		return output, XXXTodoException(fmt.Sprintf("Unsupported iterator type: %s", input.ShardIteratorType))
	}

	return output, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_ListShards.html
func (k *Kinesis) ListShards(input ListShardsInput) (*ListShardsOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	fmt.Println("ListShards", streamName)

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	// TODO: do anything with the ShardFilter?

	out := &ListShardsOutput{}
	for _, shard := range stream.Shards {
		out.Shards = append(out.Shards, APIShard{
			ShardId: shard.Id,
			HashKeyRange: APIHashKeyRange{
				StartingHashKey: shard.StartingHashKey.String(),
				EndingHashKey:   shard.EndingHashKey.String(),
			},
			SequenceNumberRange: APISequenceNumberRange{
				StartingSequenceNumber: i64toA(shard.StartingSequenceNumber),
				EndingSequenceNumber:   i64toA(shard.EndingSequenceNumber),
			},
		})
	}
	return out, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_AddTagsToStream.html
func (k *Kinesis) AddTagsToStream(input AddTagsToStreamInput) (*AddTagsToStreamOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	for tagName, tagValue := range input.Tags {
		stream.Tags[tagName] = tagValue
	}

	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_RemoveTagsFromStream.html
func (k *Kinesis) RemoveTagsFromStream(input RemoveTagsFromStreamInput) (*RemoveTagsFromStreamOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	for _, tagName := range input.TagKeys {
		delete(stream.Tags, tagName)
	}

	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_ListTagsForStream.html
func (k *Kinesis) ListTagsForStream(input ListTagsForStreamInput) (*ListTagsForStreamOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	output := &ListTagsForStreamOutput{}
	for tagName, tagValue := range stream.Tags {
		output.Tags = append(output.Tags, APITag{
			Key:   tagName,
			Value: tagValue,
		})
	}

	return output, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_IncreaseStreamRetentionPeriod.html
func (k *Kinesis) IncreaseStreamRetentionPeriod(input IncreaseStreamRetentionPeriodInput) (*IncreaseStreamRetentionPeriodOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	// TODO(zbarsky): validation
	stream.Retention = time.Duration(input.RetentionPeriodHours) * time.Hour
	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_IncreaseStreamRetentionPeriod.html
func (k *Kinesis) DecreaseStreamRetentionPeriod(input DecreaseStreamRetentionPeriodInput) (*DecreaseStreamRetentionPeriodOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	// TODO(zbarsky): validation
	stream.Retention = time.Duration(input.RetentionPeriodHours) * time.Hour
	return nil, nil
}

// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_DescribeStreamSummary.html
func (k *Kinesis) DescribeStreamSummary(input DescribeStreamSummaryInput) (*DescribeStreamSummaryOutput, *awserrors.Error) {
	streamName := input.StreamName
	if streamName == "" {
		_, streamName = arn.ExtractId(input.StreamARN)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	stream, ok := k.streams[streamName]
	if !ok {
		return nil, awserrors.ResourceNotFoundException("")
	}

	return &DescribeStreamSummaryOutput{
		StreamDescriptionSummary: APIStreamDescriptionSummary{
			ConsumerCount:           len(stream.consumersByName),
			EncryptionType:          "NONE", // TODO
			OpenShardCount:          len(stream.Shards),
			RetentionPeriodHours:    int32(stream.Retention / time.Hour),
			StreamARN:               k.arnForStream(stream.Name),
			StreamCreationTimestamp: stream.CreationTimestamp,
			StreamName:              stream.Name,
			// https://docs.aws.amazon.com/kinesis/latest/APIReference/API_StreamDescriptionSummary.html#Streams-Type-StreamDescriptionSummary-StreamStatus
			StreamStatus: "ACTIVE", // TODO
		},
	}, nil
}

func (k *Kinesis) arnForStream(streamName string) string {
	return k.arnGenerator.Generate("kinesis", "stream", streamName)
}

// These are complete HAX, they probably need to be more legit
func encodeShardIterator(streamName string, shardId string, index int) string {
	return fmt.Sprintf("%s/%s/%d", streamName, shardId, index)
}

func decodeShardIterator(
	shardIterator string,
) (
	streamName string, shardId string, start int, err error,
) {
	parts := strings.Split(shardIterator, "/")
	streamName = parts[0]
	shardId = parts[1]
	start, err = strconv.Atoi(parts[2])
	return
}
