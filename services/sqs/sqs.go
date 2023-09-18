package sqs

import (
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"aws-in-a-box/arn"
	"aws-in-a-box/awserrors"
)

type Message struct {
	Body              string
	MD5OfBody         string
	MessageAttributes map[string]APIAttribute
	// TODO: is this how we want to store it?
	MessageSystemAttributes map[string]APIAttribute
}

type Queue struct {
	// Immutable
	CreationTimestamp int64
	Attributes        map[string]string
	URL               string

	// Mutable
	Messages []*Message
	Tags     map[string]string
}

type SQS struct {
	logger       *slog.Logger
	arnGenerator arn.Generator

	mu           sync.Mutex
	queuesByName map[string]*Queue
}

type Options struct {
	Logger       *slog.Logger
	ArnGenerator arn.Generator
}

func New(options Options) *SQS {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	s := &SQS{
		logger:       options.Logger,
		arnGenerator: options.ArnGenerator,
		queuesByName: make(map[string]*Queue),
	}

	return s
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_CreateQueue.html
func (s *SQS) CreateQueue(input CreateQueueInput) (*CreateQueueOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if queue, ok := s.queuesByName[input.QueueName]; ok {
		if maps.Equal(queue.Attributes, input.Attribute) {
			return &CreateQueueOutput{
				QueueUrl: queue.URL,
			}, nil
		}
		return nil, QueueNameExists("")
	}

	url := s.getQueueUrl(input.QueueName)

	s.queuesByName[input.QueueName] = &Queue{
		Attributes: input.Attribute,
		Tags:       input.Tag,
		URL:        url,
	}

	return &CreateQueueOutput{
		QueueUrl: url,
	}, nil
}

func (s *SQS) getQueueUrl(queueName string) string {
	// TODO: We should make these not match to catch mistakes.
	// But this is expedient for now.
	return queueName
}

func (s *SQS) getQueueName(queueUrl string) string {
	// TODO: We should make these not match to catch mistakes.
	// But this is expedient for now.
	return queueUrl
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_SendMessage.html
func (s *SQS) SendMessage(input SendMessageInput) (*SendMessageOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	for name := range input.MessageSystemAttributes {
		if name != AWSTraceHeaderAttributeName {
			return nil, ValidationException("Bad MessageSystemAttribute")
		}
	}

	MD5OfBody := hexMD5([]byte(input.MessageBody))
	queue.Messages = append(queue.Messages, &Message{
		Body:                    input.MessageBody,
		MD5OfBody:               MD5OfBody,
		MessageAttributes:       input.MessageAttributes,
		MessageSystemAttributes: input.MessageSystemAttributes,
	})

	return &SendMessageOutput{
		MD5OfMessageBody: MD5OfBody,
	}, nil
}

func hexMD5(data []byte) string {
	hash := md5.Sum(data)
	return hex.EncodeToString(hash[:])
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_TagQueue.html
func (s *SQS) TagQueue(input TagQueueInput) (*TagQueueOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	for k, v := range input.Tags {
		queue.Tags[k] = v
	}

	return nil, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_UntagQueue.html
func (s *SQS) UntagQueue(input UntagQueueInput) (*UntagQueueOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	for _, key := range input.TagKeys {
		delete(queue.Tags, key)
	}

	return nil, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_GetQueueUrl.html
func (s *SQS) GetQueueUrl(input GetQueueUrlInput) (*GetQueueUrlOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &GetQueueUrlOutput{
		QueueUrl: s.getQueueUrl(input.QueueName),
	}, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_ListQueues.html
func (s *SQS) ListQueues(input ListQueuesInput) (*ListQueuesOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.MaxResults == 0 {
		input.MaxResults = 1000
	}

	output := &ListQueuesOutput{}

	for name := range s.queuesByName {
		if strings.HasPrefix(name, input.QueueNamePrefix) {
			// TODO: implement pagination
			if len(output.QueueUrls) > input.MaxResults {
				return nil, awserrors.Generate400Exception("GAH", "too many results")
			}
			output.QueueUrls = append(output.QueueUrls, s.getQueueUrl(name))
		}
	}

	return output, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_GetQueueAttributes.html
func (s *SQS) GetQueueAttributes(input GetQueueAttributesInput) (*GetQueueAttributesOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	output := &GetQueueAttributesOutput{}
	for _, name := range input.attributeNames {
		output.Attributes[name] = queue.Attributes[name]
	}

	return output, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_ListQueueTags.html
func (s *SQS) ListQueueTags(input ListQueueTagsInput) (*ListQueueTagsOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	return &ListQueueTagsOutput{
		Tags: queue.Tags,
	}, nil
}

// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_ReceiveMessage.html
func (s *SQS) ReceiveMessage(input ReceiveMessageInput) (*ReceiveMessageOutput, *awserrors.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.MaxNumberOfMessages == 0 {
		input.MaxNumberOfMessages = 10
	}
	if input.MaxNumberOfMessages < 1 || input.MaxNumberOfMessages > 10 {
		return nil, ValidationException("")
	}

	queue, ok := s.queuesByName[s.getQueueName(input.QueueUrl)]
	if !ok {
		return nil, QueueDoesNotExist("")
	}

	output := &ReceiveMessageOutput{}
	for _, message := range queue.Messages {
		// TODO: check visibility conditions

		output.Message = append(output.Message, APIMessage{
			Body:              message.Body,
			MD5OfBody:         message.MD5OfBody,
			MessageAttributes: filterAttributes(message.MessageAttributes, input.MessageAttributeNames),
		})

		if len(output.Message) == input.MaxNumberOfMessages {
			break
		}
	}

	return output, nil
}

func filterAttributes(attributes map[string]APIAttribute, attributeNames []string) map[string]APIAttribute {
	ret := make(map[string]APIAttribute)

	for k, v := range attributes {
		for _, name := range attributeNames {
			if name == "All" ||
				name == k ||
				(strings.HasSuffix(name, ".*") && strings.HasPrefix(k, name[:len(name)-2])) {
				ret[k] = v
				break
			}
		}
	}

	return ret
}
