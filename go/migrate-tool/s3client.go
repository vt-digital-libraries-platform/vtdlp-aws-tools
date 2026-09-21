package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Obj is a lightweight record of a listed S3 object.
type Obj struct {
	Key  string
	Size int64
}

// s3Lister is the subset of S3 operations plan discovery needs; kept as an
// interface so discovery logic can be tested against a fake.
type s3Lister interface {
	ListAllKeys(ctx context.Context, bucket, prefix string) ([]Obj, error)
}

// s3RW is the full set of operations the migrator needs.
type s3RW interface {
	s3Lister
	GetObjectBytes(ctx context.Context, bucket, key string) ([]byte, string, error)
	PutObjectBytes(ctx context.Context, bucket, key string, body []byte, contentType string) error
	CopyObject(ctx context.Context, bucket, oldKey, newKey string) error
	DeleteObjects(ctx context.Context, bucket string, keys []string) error
}

// S3 wraps the AWS SDK v2 S3 client with the operations this tool needs.
type S3 struct {
	client *s3.Client
}

func newS3(ctx context.Context) (*S3, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return &S3{client: s3.NewFromConfig(cfg)}, nil
}

// ListAllKeys recursively lists every object under prefix (no delimiter),
// paging through the full result set.
func (s *S3) ListAllKeys(ctx context.Context, bucket, prefix string) ([]Obj, error) {
	var out []Obj
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			out = append(out, Obj{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size)})
		}
	}
	return out, nil
}

func (s *S3) GetObjectBytes(ctx context.Context, bucket, key string) ([]byte, string, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, "", fmt.Errorf("getting %s: %w", key, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, aws.ToInt64(resp.ContentLength))
	tmp := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return buf, aws.ToString(resp.ContentType), nil
}

func (s *S3) PutObjectBytes(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("putting %s: %w", key, err)
	}
	return nil
}

func (s *S3) CopyObject(ctx context.Context, bucket, oldKey, newKey string) error {
	source := bucket + "/" + escapeCopySource(oldKey)
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(newKey),
		CopySource: aws.String(source),
	})
	if err != nil {
		return fmt.Errorf("copying %s -> %s: %w", oldKey, newKey, err)
	}
	return nil
}

// escapeCopySource percent-encodes characters CopySource requires escaped
// (S3 wants the source key URL-encoded; spaces and a few punctuation marks
// show up in these keys, e.g. "full/160,/0/default.jpg").
func escapeCopySource(key string) string {
	r := strings.NewReplacer(
		" ", "%20",
		",", "%2C",
	)
	return r.Replace(key)
}

func (s *S3) DeleteObjects(ctx context.Context, bucket string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	const batchSize = 1000
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		ids := make([]types.ObjectIdentifier, len(batch))
		for j, k := range batch {
			ids[j] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids},
		})
		if err != nil {
			return fmt.Errorf("deleting batch: %w", err)
		}
	}
	return nil
}
