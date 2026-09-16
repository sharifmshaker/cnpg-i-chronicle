/*
Copyright 2026 The cnpg-i-chronicle Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package store

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// s3Backend implements Backend against any S3-compatible endpoint, including
// the RustFS store the dev environment runs.
type s3Backend struct {
	client *s3.Client
	bucket string
}

// S3Options is everything needed to reach an S3-compatible endpoint.
type S3Options struct {
	Bucket          string
	Region          string
	EndpointURL     string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// CABundle is a PEM bundle for a private endpoint's TLS certificate.
	CABundle []byte

	// InheritFromIAMRole skips static credentials and uses the ambient chain
	// (IRSA, instance profile).
	InheritFromIAMRole bool
}

// NewS3Backend builds an S3 backend.
func NewS3Backend(ctx context.Context, options S3Options) (Backend, error) {
	if options.Bucket == "" {
		return nil, errors.New("s3 backend requires a bucket")
	}

	// S3 always wants a region even when the endpoint ignores it; self-hosted
	// stores such as RustFS accept any value.
	region := options.Region
	if region == "" {
		region = "us-east-1"
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}

	if !options.InheritFromIAMRole && options.AccessKeyID != "" {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				options.AccessKeyID, options.SecretAccessKey, options.SessionToken),
		))
	}

	if len(options.CABundle) > 0 {
		httpClient, err := httpClientWithCA(options.CABundle)
		if err != nil {
			return nil, err
		}
		loadOptions = append(loadOptions, awsconfig.WithHTTPClient(httpClient))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("while building AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if options.EndpointURL != "" {
			o.BaseEndpoint = aws.String(options.EndpointURL)
			// Virtual-host addressing requires per-bucket DNS, which
			// self-hosted endpoints such as RustFS do not provide.
			o.UsePathStyle = true
		}
	})

	return &s3Backend{client: client, bucket: options.Bucket}, nil
}

func httpClientWithCA(bundle []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, errors.New("endpointCA did not contain any valid PEM certificate")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport}, nil
}

// maxObjectSize bounds a single Get. Snapshots are a few kilobytes; this is
// generous enough that no real one approaches it, and small enough that a
// surprise at a snapshot key cannot exhaust the process.
const maxObjectSize = 4 << 20 // 4 MiB

func (b *s3Backend) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: s3://%s/%s", ErrNotFound, b.bucket, key)
		}
		return nil, fmt.Errorf("while reading s3://%s/%s: %w", b.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()

	// Bounded, because this is reachable from the admission webhook: an
	// oversized object at a snapshot key — a shared bucket, a misconfigured
	// prefix, anything that put something else there — would otherwise be read
	// wholesale into the process that every Cluster creation depends on.
	//
	// A snapshot is a subset of a Cluster spec, and Kubernetes objects are
	// themselves bounded well below this, so the cap cannot reject a legitimate
	// document. Reading one byte past it is what tells truncation apart from a
	// document that merely happens to be exactly the limit.
	data, err := io.ReadAll(io.LimitReader(out.Body, maxObjectSize+1))
	if err != nil {
		return nil, fmt.Errorf("while reading body of s3://%s/%s: %w", b.bucket, key, err)
	}
	if len(data) > maxObjectSize {
		return nil, fmt.Errorf(
			"s3://%s/%s is larger than %d bytes, which is not a snapshot this plugin "+
				"wrote. Check that the destinationPath and prefix do not overlap other data",
			b.bucket, key, maxObjectSize)
	}
	return data, nil
}

func (b *s3Backend) Put(ctx context.Context, key string, data []byte) error {
	err := b.putObject(ctx, key, data)
	if err == nil {
		return nil
	}
	if !isNoSuchBucket(err) {
		return fmt.Errorf("while writing s3://%s/%s: %w", b.bucket, key, err)
	}

	// The bucket does not exist yet. barman-cloud's S3 client creates it on
	// first write, and self-hosted endpoints such as RustFS never
	// create one implicitly, so a store pointed at a fresh bucket would fail
	// here for a reason the user reasonably expects to be handled. Create it
	// once and retry, exactly once, so a genuinely broken store still surfaces.
	if createErr := b.createBucket(ctx); createErr != nil {
		return fmt.Errorf(
			"bucket %q does not exist and could not be created (%v). Create it "+
				"beforehand, or grant s3:CreateBucket, then retry: %w",
			b.bucket, createErr, err)
	}

	if err := b.putObject(ctx, key, data); err != nil {
		return fmt.Errorf("while writing s3://%s/%s after creating the bucket: %w", b.bucket, key, err)
	}
	return nil
}

func (b *s3Backend) putObject(ctx context.Context, key string, data []byte) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// createBucket creates the bucket, tolerating a concurrent creation by another
// replica or by barman.
func (b *s3Backend) createBucket(ctx context.Context) error {
	_, err := b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b.bucket)})
	if err == nil {
		return nil
	}

	var alreadyOwned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &alreadyOwned) {
		return nil
	}
	var alreadyExists *types.BucketAlreadyExists
	if errors.As(err, &alreadyExists) {
		return nil
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return nil
		}
	}
	return err
}

func (b *s3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string

	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("while listing s3://%s/%s: %w", b.bucket, prefix, err)
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				keys = append(keys, *object.Key)
			}
		}
	}

	// S3 returns keys in lexical order per page, but sorting explicitly means
	// selection does not depend on that guarantee holding for every
	// S3-compatible implementation.
	sort.Strings(keys)
	return keys, nil
}

// Check lists at most one key under the destination path.
//
// Listing rather than reading a known key, because a read of a missing key
// without s3:ListBucket is answered with 403, not 404 — so listing is a
// permission capture already depends on, and the probe needs nothing new.
func (b *s3Backend) Check(ctx context.Context, prefix string) error {
	_, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	switch {
	case err == nil:
		return nil
	case isNoSuchBucket(err):
		return fmt.Errorf("%w: %s", ErrBucketNotFound, b.bucket)
	default:
		return fmt.Errorf("cannot list s3://%s/%s: %s", b.bucket, prefix, describeS3Error(err))
	}
}

// describeS3Error reduces an S3 error to its code and message.
//
// The SDK's own rendering carries a request ID and host ID that differ on every
// attempt. That is useful in a log and harmful in a status condition, where a
// message that changes on each resync rewrites the object every time.
func describeS3Error(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s: %s", apiErr.ErrorCode(), apiErr.ErrorMessage())
	}
	return err.Error()
}

func (b *s3Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("while deleting s3://%s/%s: %w", b.bucket, key, err)
	}
	return nil
}

// isNotFound recognises a missing key across S3 implementations, which disagree
// on whether they return NoSuchKey, NotFound or a bare 404.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	// Some implementations answer a bare 404 with no error code at all.
	var response *awshttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}

// isNoSuchBucket distinguishes a missing bucket from a missing object. Both
// surface as 404, but only one of them is worth trying to fix.
func isNoSuchBucket(err error) bool {
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &noSuchBucket) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchBucket"
	}
	return false
}
