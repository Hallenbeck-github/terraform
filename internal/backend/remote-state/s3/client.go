// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	multierror "github.com/hashicorp/go-multierror"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/terraform/internal/states/remote"
	"github.com/hashicorp/terraform/internal/states/statemgr"
)

const (
	// s3EncryptionAlgorithm = s3types.ServerSideEncryptionAes256
	s3EncryptionAlgorithm = "AES256"

	// Store the last saved serial in dynamo with this suffix for consistency checks.
	stateIDSuffix = "-md5"
)

type RemoteClient struct {
	s3Client              *s3.Client
	dynClient             *dynamodb.Client
	bucketName            string
	path                  string
	serverSideEncryption  bool
	customerEncryptionKey []byte
	acl                   string
	kmsKeyID              string
	ddbTable              string
}

var (
	// The amount of time we will retry a state waiting for it to match the
	// expected checksum.
	consistencyRetryTimeout = 10 * time.Second

	// delay when polling the state
	consistencyRetryPollInterval = 2 * time.Second
)

// test hook called when checksums don't match
var testChecksumHook func()

func (c *RemoteClient) Get() (payload *remote.Payload, err error) {
	ctx := context.TODO()

	deadline := time.Now().Add(consistencyRetryTimeout)

	// If we have a checksum, and the returned payload doesn't match, we retry
	// up until deadline.
	for {
		payload, err = c.get(ctx)
		if err != nil {
			return nil, err
		}

		// If the remote state was manually removed the payload will be nil,
		// but if there's still a digest entry for that state we will still try
		// to compare the MD5 below.
		var digest []byte
		if payload != nil {
			digest = payload.MD5
		}

		// verify that this state is what we expect
		if expected, err := c.getMD5(ctx); err != nil {
			log.Printf("[WARN] failed to fetch state md5: %s", err)
		} else if len(expected) > 0 && !bytes.Equal(expected, digest) {
			log.Printf("[WARN] state md5 mismatch: expected '%x', got '%x'", expected, digest)

			if testChecksumHook != nil {
				testChecksumHook()
			}

			if time.Now().Before(deadline) {
				time.Sleep(consistencyRetryPollInterval)
				log.Println("[INFO] retrying S3 RemoteClient.Get...")
				continue
			}

			return nil, fmt.Errorf(errBadChecksumFmt, digest)
		}

		break
	}

	return payload, err
}

func (c *RemoteClient) get(ctx context.Context) (*remote.Payload, error) {
	headInput := &s3.HeadObjectInput{
		Bucket: aws.String(c.bucketName),
		Key:    aws.String(c.path),
	}
	if c.serverSideEncryption && c.customerEncryptionKey != nil {
		headInput.SSECustomerKey = aws.String(base64.StdEncoding.EncodeToString(c.customerEncryptionKey))
		headInput.SSECustomerAlgorithm = aws.String(s3EncryptionAlgorithm)
		headInput.SSECustomerKeyMD5 = aws.String(c.getSSECustomerKeyMD5())
	}

	headOut, err := c.s3Client.HeadObject(ctx, headInput)
	if err != nil {
		switch {
		case IsA[*s3types.NoSuchBucket](err):
			return nil, fmt.Errorf(errS3NoSuchBucket, err)
		case IsA[*s3types.NotFound](err):
			return nil, nil
		}
		return nil, err
	}

	// Pre-allocate the full buffer to avoid re-allocations and GC
	buf := make([]byte, int(headOut.ContentLength))
	w := manager.NewWriteAtBuffer(buf)

	downloadInput := &s3.GetObjectInput{
		Bucket: aws.String(c.bucketName),
		Key:    aws.String(c.path),
	}
	if c.serverSideEncryption && c.customerEncryptionKey != nil {
		downloadInput.SSECustomerKey = aws.String(base64.StdEncoding.EncodeToString(c.customerEncryptionKey))
		downloadInput.SSECustomerAlgorithm = aws.String(s3EncryptionAlgorithm)
		downloadInput.SSECustomerKeyMD5 = aws.String(c.getSSECustomerKeyMD5())
	}

	downloader := manager.NewDownloader(c.s3Client)

	_, err = downloader.Download(ctx, w, downloadInput)
	if err != nil {
		switch {
		case IsA[*s3types.NoSuchBucket](err):
			return nil, fmt.Errorf(errS3NoSuchBucket, err)
		case IsA[*s3types.NoSuchKey](err):
			return nil, nil
		}
		return nil, err
	}

	sum := md5.Sum(w.Bytes())
	payload := &remote.Payload{
		Data: w.Bytes(),
		MD5:  sum[:],
	}

	// If there was no data, then return nil
	if len(payload.Data) == 0 {
		return nil, nil
	}

	return payload, nil
}

func (c *RemoteClient) Put(data []byte) error {
	ctx := context.TODO()

	contentType := "application/json"

	input := &s3.PutObjectInput{
		ContentType: aws.String(contentType),
		Body:        bytes.NewReader(data),
		Bucket:      aws.String(c.bucketName),
		Key:         aws.String(c.path),
	}

	if c.serverSideEncryption {
		if c.kmsKeyID != "" {
			input.SSEKMSKeyId = aws.String(c.kmsKeyID)
			input.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		} else if c.customerEncryptionKey != nil {
			input.SSECustomerKey = aws.String(base64.StdEncoding.EncodeToString(c.customerEncryptionKey))
			input.SSECustomerAlgorithm = aws.String(string(s3EncryptionAlgorithm))
			input.SSECustomerKeyMD5 = aws.String(c.getSSECustomerKeyMD5())
		} else {
			input.ServerSideEncryption = s3EncryptionAlgorithm
		}
	}

	if c.acl != "" {
		input.ACL = s3types.ObjectCannedACL(c.acl)
	}

	log.Printf("[DEBUG] Uploading remote state to S3: %#v", input)

	uploader := manager.NewUploader(c.s3Client)
	_, err := uploader.Upload(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to upload state: %s", err)
	}

	sum := md5.Sum(data)
	if err := c.putMD5(ctx, sum[:]); err != nil {
		// if this errors out, we unfortunately have to error out altogether,
		// since the next Get will inevitably fail.
		return fmt.Errorf("failed to store state MD5: %s", err)
	}

	return nil
}

func (c *RemoteClient) Delete() error {
	ctx := context.TODO()

	_, err := c.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucketName),
		Key:    aws.String(c.path),
	})

	if err != nil {
		return err
	}

	if err := c.deleteMD5(ctx); err != nil {
		log.Printf("error deleting state md5: %s", err)
	}

	return nil
}

func (c *RemoteClient) Lock(info *statemgr.LockInfo) (string, error) {
	ctx := context.TODO()

	if c.ddbTable == "" {
		return "", nil
	}

	info.Path = c.lockPath()

	if info.ID == "" {
		lockID, err := uuid.GenerateUUID()
		if err != nil {
			return "", err
		}

		info.ID = lockID
	}

	putParams := &dynamodb.PutItemInput{
		Item: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath(),
			},
			"Info": &dynamodbtypes.AttributeValueMemberS{
				Value: string(info.Marshal()),
			},
		},
		TableName:           aws.String(c.ddbTable),
		ConditionExpression: aws.String("attribute_not_exists(LockID)"),
	}
	_, err := c.dynClient.PutItem(ctx, putParams)

	if err != nil {
		lockInfo, infoErr := c.getLockInfo(ctx)
		if infoErr != nil {
			err = multierror.Append(err, infoErr)
		}

		lockErr := &statemgr.LockError{
			Err:  err,
			Info: lockInfo,
		}
		return "", lockErr
	}

	return info.ID, nil
}

func (c *RemoteClient) getMD5(ctx context.Context) ([]byte, error) {
	if c.ddbTable == "" {
		return nil, nil
	}

	getParams := &dynamodb.GetItemInput{
		Key: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath() + stateIDSuffix,
			},
		},
		ProjectionExpression: aws.String("LockID, Digest"),
		TableName:            aws.String(c.ddbTable),
		ConsistentRead:       aws.Bool(true),
	}

	resp, err := c.dynClient.GetItem(ctx, getParams)
	if err != nil {
		return nil, err
	}

	var val string
	if v, ok := resp.Item["Digest"]; ok {
		if v, ok := v.(*dynamodbtypes.AttributeValueMemberS); ok {
			val = v.Value
		}
	}

	sum, err := hex.DecodeString(val)
	if err != nil || len(sum) != md5.Size {
		return nil, errors.New("invalid md5")
	}

	return sum, nil
}

// store the hash of the state so that clients can check for stale state files.
func (c *RemoteClient) putMD5(ctx context.Context, sum []byte) error {
	if c.ddbTable == "" {
		return nil
	}

	if len(sum) != md5.Size {
		return errors.New("invalid payload md5")
	}

	putParams := &dynamodb.PutItemInput{
		Item: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath() + stateIDSuffix,
			},
			"Digest": &dynamodbtypes.AttributeValueMemberS{
				Value: hex.EncodeToString(sum),
			},
		},
		TableName: aws.String(c.ddbTable),
	}
	_, err := c.dynClient.PutItem(ctx, putParams)
	if err != nil {
		log.Printf("[WARN] failed to record state serial in dynamodb: %s", err)
	}

	return nil
}

// remove the hash value for a deleted state
func (c *RemoteClient) deleteMD5(ctx context.Context) error {
	if c.ddbTable == "" {
		return nil
	}

	params := &dynamodb.DeleteItemInput{
		Key: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath() + stateIDSuffix,
			},
		},
		TableName: aws.String(c.ddbTable),
	}
	if _, err := c.dynClient.DeleteItem(ctx, params); err != nil {
		return err
	}
	return nil
}

func (c *RemoteClient) getLockInfo(ctx context.Context) (*statemgr.LockInfo, error) {
	getParams := &dynamodb.GetItemInput{
		Key: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath(),
			},
		},
		ProjectionExpression: aws.String("LockID, Info"),
		TableName:            aws.String(c.ddbTable),
		ConsistentRead:       aws.Bool(true),
	}

	resp, err := c.dynClient.GetItem(ctx, getParams)
	if err != nil {
		return nil, err
	}

	var infoData string
	if v, ok := resp.Item["Info"]; ok {
		if v, ok := v.(*dynamodbtypes.AttributeValueMemberS); ok {
			infoData = v.Value
		}
	}

	lockInfo := &statemgr.LockInfo{}
	err = json.Unmarshal([]byte(infoData), lockInfo)
	if err != nil {
		return nil, err
	}

	return lockInfo, nil
}

func (c *RemoteClient) Unlock(id string) error {
	ctx := context.TODO()

	if c.ddbTable == "" {
		return nil
	}

	lockErr := &statemgr.LockError{}

	// TODO: store the path and lock ID in separate fields, and have proper
	// projection expression only delete the lock if both match, rather than
	// checking the ID from the info field first.
	lockInfo, err := c.getLockInfo(ctx)
	if err != nil {
		lockErr.Err = fmt.Errorf("failed to retrieve lock info: %s", err)
		return lockErr
	}
	lockErr.Info = lockInfo

	if lockInfo.ID != id {
		lockErr.Err = fmt.Errorf("lock id %q does not match existing lock", id)
		return lockErr
	}

	params := &dynamodb.DeleteItemInput{
		Key: map[string]dynamodbtypes.AttributeValue{
			"LockID": &dynamodbtypes.AttributeValueMemberS{
				Value: c.lockPath(),
			},
		},
		TableName: aws.String(c.ddbTable),
	}
	_, err = c.dynClient.DeleteItem(ctx, params)

	if err != nil {
		lockErr.Err = err
		return lockErr
	}
	return nil
}

func (c *RemoteClient) lockPath() string {
	return fmt.Sprintf("%s/%s", c.bucketName, c.path)
}

func (c *RemoteClient) getSSECustomerKeyMD5() string {
	b := md5.Sum(c.customerEncryptionKey)
	return base64.StdEncoding.EncodeToString(b[:])
}

const errBadChecksumFmt = `state data in S3 does not have the expected content.

This may be caused by unusually long delays in S3 processing a previous state
update.  Please wait for a minute or two and try again. If this problem
persists, and neither S3 nor DynamoDB are experiencing an outage, you may need
to manually verify the remote state and update the Digest value stored in the
DynamoDB table to the following value: %x
`

const errS3NoSuchBucket = `S3 bucket does not exist.

The referenced S3 bucket must have been previously created. If the S3 bucket
was created within the last minute, please wait for a minute or two and try
again.

Error: %s
`
